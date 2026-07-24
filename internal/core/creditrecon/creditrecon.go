// Package creditrecon is a background ticker that reconciles higgsgo's
// locally-recorded per-account credit spend against the upstream
// higgsfield credit ledger's aggregate. It mirrors the other tickers
// (claimer, invoicewatch, promowatch, monthreset): one struct, one ticker,
// one goroutine launched by main.go.
//
// Flow per tick (default cadence 24h — production intent is once a day at
// 00:15 UTC, but the ticker is a plain interval loop for simplicity and
// test determinism): compute the target month → List(active) → fan out
// bounded by Concurrency → for each account GET
// /workspaces/credit-ledger/statistics for the window and SUM local
// usage_events.charged_credits_h for the same window → compare and, if
// the absolute difference exceeds threshold, fire a Warn notification.
//
// Target-month picking is deliberately conservative: the ticker uses the
// month that owns "yesterday" (T-24h). On the 1st of every month that
// means "the just-completed calendar month"; every other day it means
// "the current month, up to right now" as a half-open window
// [YYYY-MM-01, next-YYYY-MM-01). That gives a rolling in-month sanity
// check and a clean, full-month reconciliation on the 1st without any
// special-cased day-of-month logic in the caller.
//
// Alerts are deduplicated in-memory keyed by (accountID, targetMonth) so
// the same account never sees two alerts about the same month. A process
// restart clears the mute map — acceptable trade for zero schema cost.
// Alert-only: no automatic correction, no writes back to any store.
//
// Units: the upstream endpoint returns "credits" (integer, same unit as
// wallet.credits_balance); higgsgo stores charged_credits_h in hundredths
// (×100). The reconciler multiplies upstream credits by 100 before
// comparing so both sides live in the same _h space.
package creditrecon

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

const (
	defaultInterval             = 24 * time.Hour
	defaultConcurrency          = 3
	defaultAbsoluteFloorCredits = 100  // 100 credits → 10 000 _h floor
	defaultRelativePct          = 0.05 // 5 % of the upstream total (in _h)
)

// Reconciler compares upstream credit-ledger totals against the locally-
// recorded usage_events.charged_credits_h sum for a rolling monthly window
// and alerts (Warn) via the notifier chain when the two diverge by more
// than the configured threshold. Alert-only: never mutates any state.
type Reconciler struct {
	Accounts    ports.AccountStore
	Usage       ports.UsageEventStore
	Upstream    *upstream.Client
	Notifier    ports.Notifier
	Logger      *slog.Logger
	Interval    time.Duration // zero selects defaultInterval
	Concurrency int           // zero selects defaultConcurrency

	// AbsoluteFloorCredits is the minimum diff (in credits, i.e. the same
	// unit upstream returns) that triggers an alert. Below this we treat
	// the mismatch as noise. Zero selects defaultAbsoluteFloorCredits.
	AbsoluteFloorCredits int
	// RelativePct is the fraction of the upstream total (in _h) above
	// which the diff is considered material. The effective threshold is
	// max(absoluteFloor_h, upstream_h * RelativePct). Zero selects
	// defaultRelativePct.
	RelativePct float64

	mu    sync.Mutex
	muted map[string]time.Time // "accountID|YYYY-MM" -> last alert time
	now   func() time.Time     // injectable clock for tests
}

// New builds a Reconciler with defaults filled. Interval / Concurrency /
// AbsoluteFloorCredits / RelativePct can be overridden by the caller after
// construction.
func New(accounts ports.AccountStore, usage ports.UsageEventStore, up *upstream.Client, ntf ports.Notifier, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		Accounts:             accounts,
		Usage:                usage,
		Upstream:             up,
		Notifier:             ntf,
		Logger:               logger,
		Interval:             defaultInterval,
		Concurrency:          defaultConcurrency,
		AbsoluteFloorCredits: defaultAbsoluteFloorCredits,
		RelativePct:          defaultRelativePct,
		muted:                make(map[string]time.Time),
		now:                  time.Now,
	}
}

// Run blocks until ctx is canceled. Intended as `go r.Run(ctx)`. A nil
// receiver or unwired dependency returns immediately so callers need not
// guard the launch.
func (r *Reconciler) Run(ctx context.Context) {
	if r == nil || r.Accounts == nil || r.Usage == nil || r.Upstream == nil || r.Notifier == nil {
		return
	}
	if r.Interval <= 0 {
		r.Interval = defaultInterval
	}
	if r.Concurrency <= 0 {
		r.Concurrency = defaultConcurrency
	}
	if r.AbsoluteFloorCredits <= 0 {
		r.AbsoluteFloorCredits = defaultAbsoluteFloorCredits
	}
	if r.RelativePct <= 0 {
		r.RelativePct = defaultRelativePct
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.muted == nil {
		r.muted = make(map[string]time.Time)
	}
	if r.Logger != nil {
		r.Logger.Info("creditrecon starting", slog.Duration("interval", r.Interval))
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	r.once(ctx) // immediate first pass so a divergence isn't stalled a full interval
	for {
		select {
		case <-ctx.Done():
			if r.Logger != nil {
				r.Logger.Info("creditrecon stopping")
			}
			return
		case <-ticker.C:
			r.once(ctx)
		}
	}
}

// TriggerOnce runs a single reconcile pass synchronously. Used by tests
// and (optionally) an admin trigger endpoint.
func (r *Reconciler) TriggerOnce(ctx context.Context) { r.once(ctx) }

// once fans out reconOne over every active account for the current target
// month, bounded by Concurrency. One failing account never blocks the
// others.
func (r *Reconciler) once(ctx context.Context) {
	if r == nil || r.Accounts == nil || r.Usage == nil || r.Upstream == nil || r.Notifier == nil {
		return
	}
	start, end := r.targetWindow()
	accounts, err := r.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("creditrecon list accounts", slog.String("err", err.Error()))
		}
		return
	}
	if len(accounts) == 0 {
		return
	}
	conc := r.Concurrency
	if conc <= 0 {
		conc = defaultConcurrency
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i := range accounts {
		acc := accounts[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.reconOne(ctx, &acc, start, end)
		}()
	}
	wg.Wait()
}

// reconOne performs one account's reconciliation over the [start, end)
// window. Both endpoints (upstream stats + local sum) are best-effort:
// failure on one account is logged at warn and doesn't block the others.
func (r *Reconciler) reconOne(ctx context.Context, acc *domain.Account, start, end time.Time) {
	// Upstream endpoint takes ISO YYYY-MM-DD strings; the exact end
	// semantics (inclusive/exclusive) don't matter for the "full previous
	// month" case because start and end lie on month boundaries. For an
	// in-month tick we intentionally pass "end - 1 day" so upstream sees
	// an inclusive close-of-yesterday, matching the local half-open cutoff.
	startStr := start.UTC().Format("2006-01-02")
	endStr := end.UTC().Add(-24 * time.Hour).Format("2006-01-02")

	stats, err := r.Upstream.FetchCreditLedgerStatistics(ctx, acc, startStr, endStr)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("creditrecon fetch upstream statistics",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	if stats == nil {
		return // defensive — FetchCreditLedgerStatistics returns a zero value, not nil
	}
	upstreamCreditsH := stats.TotalCreditsSpent * 100 // credits → hundredths

	localH, err := r.Usage.SumChargedCreditsHForAccount(ctx, acc.ID, start, end)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("creditrecon sum local usage",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}

	diffH := upstreamCreditsH - localH
	absDiffH := diffH
	if absDiffH < 0 {
		absDiffH = -absDiffH
	}
	threshold := r.thresholdH(upstreamCreditsH)
	if absDiffH <= threshold {
		return // within tolerance — nothing to alert
	}

	monthKey := start.UTC().Format("2006-01")
	if !r.reserveAlert(acc.ID + "|" + monthKey) {
		return // already alerted this account for this month
	}
	msg := ports.Notification{
		Level: ports.LevelWarn,
		Title: "Credit ledger reconciliation mismatch",
		Body:  "An active account's locally-recorded credit spend diverged from the upstream credit ledger for the target month beyond the configured threshold; investigate metering / refund flow.",
		Tags: map[string]string{
			"account_id":       acc.ID,
			"email":            acc.Email,
			"month":            monthKey,
			"window_start":     startStr,
			"window_end":       endStr,
			"upstream_credits": strconv.FormatInt(stats.TotalCreditsSpent, 10),
			"local_credits":    strconv.FormatInt(localH/100, 10),
			"upstream_h":       strconv.FormatInt(upstreamCreditsH, 10),
			"local_h":          strconv.FormatInt(localH, 10),
			"diff_h":           strconv.FormatInt(diffH, 10),
			"threshold_h":      strconv.FormatInt(threshold, 10),
			"jobs_upstream":    strconv.FormatInt(stats.JobsCreated, 10),
		},
	}
	if err := r.Notifier.Send(ctx, msg); err != nil && r.Logger != nil {
		r.Logger.Warn("creditrecon notify failed",
			slog.String("account_id", acc.ID),
			slog.String("err", err.Error()))
	}
}

// thresholdH returns the diff threshold in hundredths units, computed as
// max(absoluteFloor_h, upstream_h * RelativePct). Using the upstream side
// of the diff (not local) as the relative base keeps the threshold stable
// against a runaway local sum bug — a bad local write would otherwise
// widen the tolerance and mask the very problem the reconciler is meant
// to catch.
func (r *Reconciler) thresholdH(upstreamH int64) int64 {
	floorH := int64(r.AbsoluteFloorCredits) * 100
	if upstreamH <= 0 {
		return floorH
	}
	relH := int64(float64(upstreamH) * r.RelativePct)
	if relH > floorH {
		return relH
	}
	return floorH
}

// targetWindow computes the [start, end) UTC half-open interval the tick
// reconciles. "Yesterday" (T-24h) picks the month: on the 1st this yields
// the just-completed month; on any other day it yields the current month.
// This gives a rolling in-month sanity check plus a clean full-month
// reconciliation on the 1st, driven by a single fixed rule.
func (r *Reconciler) targetWindow() (time.Time, time.Time) {
	now := r.now().UTC()
	ref := now.Add(-24 * time.Hour)
	start := time.Date(ref.Year(), ref.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	return start, end
}

// reserveAlert returns true (and records the fire time) when the given
// dedup key has not fired before; false when it has. Because target-month
// keys change once a month, a natural eviction happens by staleness; we
// still prune entries older than 60 days on each call so a long-running
// process doesn't accumulate keys for accounts that vanished from the
// pool. Keeps the muted map bounded without any goroutine.
func (r *Reconciler) reserveAlert(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if _, ok := r.muted[key]; ok {
		return false
	}
	for k, t := range r.muted {
		if now.Sub(t) >= 60*24*time.Hour {
			delete(r.muted, k)
		}
	}
	r.muted[key] = now
	return true
}
