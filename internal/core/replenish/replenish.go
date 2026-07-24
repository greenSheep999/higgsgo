// Package replenish is a background ticker that watches the account pool
// for depletion signals and fires operator notifications through a
// ports.Notifier. It mirrors the repo's other tickers (refresher,
// claimer, failover.Recoverer): one struct, one ticker, one goroutine
// launched by main.go.
//
// Signals implemented: S1 unlim-pool depth per job_set_type, S2 credit
// exhaustion ratio, S5 plans ending soon. S3 (grace/declined) and S4
// (unclaimed gift value) are stubbed pending their data dependencies
// (grace_status column, account_gifts table) — see the stub methods.
//
// Deduplication is in-memory: the same signal fires at most once per 24h.
// A process restart re-arms every signal (one extra alert), which is an
// acceptable trade for zero schema cost.
package replenish

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

const (
	defaultInterval = time.Minute
	dedupWindow     = 24 * time.Hour
)

// Thresholds mirrors the operator-tunable knobs from config.ReplenishConfig.
type Thresholds struct {
	CreditFloor         int64 // credits_balance + subscription_balance/100 below this = exhausted
	CreditExhaustionPct float64
	PlanEndingDays      int
	PlanEndingThreshold int
	MinUnlimPoolSize    int
	WatchedJobSetTypes  []string
}

// Replenish scans the pool each Interval and alerts on depletion.
type Replenish struct {
	Accounts ports.AccountStore
	Notifier ports.Notifier
	Logger   *slog.Logger
	Interval time.Duration
	Cfg      Thresholds

	mu    sync.Mutex
	muted map[string]time.Time // signalKey -> last fired
	now   func() time.Time     // injectable clock for tests
}

// New builds a Replenish with defaults filled.
func New(accounts ports.AccountStore, ntf ports.Notifier, logger *slog.Logger, cfg Thresholds) *Replenish {
	return &Replenish{
		Accounts: accounts,
		Notifier: ntf,
		Logger:   logger,
		Interval: defaultInterval,
		Cfg:      cfg,
		muted:    make(map[string]time.Time),
		now:      time.Now,
	}
}

// Run blocks until ctx is canceled. Intended as `go r.Run(ctx)`.
func (r *Replenish) Run(ctx context.Context) {
	if r == nil || r.Accounts == nil || r.Notifier == nil {
		return
	}
	if r.Interval <= 0 {
		r.Interval = defaultInterval
	}
	if r.Logger != nil {
		r.Logger.Info("replenish ticker starting", slog.Duration("interval", r.Interval))
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	r.once(ctx)
	for {
		select {
		case <-ctx.Done():
			if r.Logger != nil {
				r.Logger.Info("replenish ticker stopping")
			}
			return
		case <-ticker.C:
			r.once(ctx)
		}
	}
}

// TriggerOnce runs a single scan synchronously (tests + admin trigger).
func (r *Replenish) TriggerOnce(ctx context.Context) { r.once(ctx) }

// once runs all signals. Each is isolated: a failing query logs and the
// others still run.
func (r *Replenish) once(ctx context.Context) {
	if r == nil || r.Accounts == nil || r.Notifier == nil {
		return
	}
	accounts, err := r.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("replenish list accounts", slog.String("err", err.Error()))
		}
	} else {
		r.checkCreditExhaustion(ctx, accounts) // S2
		r.checkPlansEnding(ctx, accounts)      // S5
		r.checkGraceAccounts(ctx, accounts)    // S3
	}
	r.checkUnlimPools(ctx)     // S1
	r.checkUnclaimedGifts(ctx) // S4 (stub)
}

// checkCreditExhaustion (S2): fraction of the active pool whose usable
// credit is below CreditFloor. subscription_balance is credits×100 and is
// the reliable budget signal (credits_balance transiently hits 0 during a
// freeze window — see domain notes).
func (r *Replenish) checkCreditExhaustion(ctx context.Context, accounts []domain.Account) {
	if len(accounts) == 0 || r.Cfg.CreditExhaustionPct <= 0 {
		return
	}
	exhausted := 0
	for i := range accounts {
		usable := accounts[i].CreditsBalance + accounts[i].SubscriptionBalance/100
		if usable < r.Cfg.CreditFloor {
			exhausted++
		}
	}
	ratio := float64(exhausted) / float64(len(accounts))
	if ratio > r.Cfg.CreditExhaustionPct {
		r.fire(ctx, "S2:credit_exhaustion", ports.LevelWarn,
			"Account pool credit running low",
			"A large share of active accounts are near-empty; top up or add accounts.",
			map[string]string{
				"exhausted": itoa(exhausted),
				"active":    itoa(len(accounts)),
				"ratio":     ftoa(ratio),
			})
	}
}

// checkPlansEnding (S5): active accounts whose plan_ends_at falls within
// PlanEndingDays. plan_ends_at is stored as an RFC3339 string; a parse
// failure or zero time is skipped.
func (r *Replenish) checkPlansEnding(ctx context.Context, accounts []domain.Account) {
	if r.Cfg.PlanEndingDays <= 0 {
		return
	}
	cutoff := r.now().Add(time.Duration(r.Cfg.PlanEndingDays) * 24 * time.Hour)
	ending := 0
	for i := range accounts {
		t := accounts[i].PlanEndsAt
		if t.IsZero() {
			continue
		}
		if t.Before(cutoff) {
			ending++
		}
	}
	if ending > r.Cfg.PlanEndingThreshold {
		r.fire(ctx, "S5:plans_ending", ports.LevelWarn,
			"Subscriptions expiring soon",
			"Multiple accounts have plans ending within the alert window; renew or replace.",
			map[string]string{
				"count": itoa(ending),
				"days":  itoa(r.Cfg.PlanEndingDays),
			})
	}
}

// checkUnlimPools (S1): for each watched job_set_type, alert when the
// count of active accounts holding a live unlim bundle drops below
// MinUnlimPoolSize. Empty watch list skips the signal entirely.
func (r *Replenish) checkUnlimPools(ctx context.Context) {
	if len(r.Cfg.WatchedJobSetTypes) == 0 || r.Cfg.MinUnlimPoolSize <= 0 {
		return
	}
	counts, err := r.Accounts.CountActiveUnlimByJST(ctx)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("replenish count unlim", slog.String("err", err.Error()))
		}
		return
	}
	for _, jst := range r.Cfg.WatchedJobSetTypes {
		n := counts[jst] // absent = 0
		if n < r.Cfg.MinUnlimPoolSize {
			r.fire(ctx, "S1:"+jst, ports.LevelWarn,
				"Unlim pool draining",
				"Active unlim accounts for a watched model fell below the floor; claim/buy more bundles.",
				map[string]string{
					"job_set_type": jst,
					"pool":         itoa(n),
					"floor":        itoa(r.Cfg.MinUnlimPoolSize),
				})
		}
	}
}

// checkGraceAccounts (S3): count active accounts flagged upstream —
// payment-risk grace_status, a blocked_at / suspended_at timestamp, or a
// paused subscription. Any such account is a "go look at this now"
// signal (a paying account you can't use). Two separate dedup keys keep
// payment-risk (grace) and hard-flag (blocked/suspended/paused) alerts
// independent.
func (r *Replenish) checkGraceAccounts(ctx context.Context, accounts []domain.Account) {
	grace, flagged := 0, 0
	for i := range accounts {
		if accounts[i].GraceStatus != "" {
			grace++
		}
		if accounts[i].BlockedAt != "" || accounts[i].SuspendedAt != "" || accounts[i].IsPaused {
			flagged++
		}
	}
	if grace > 0 {
		r.fire(ctx, "S3:grace", ports.LevelWarn,
			"Accounts in payment-risk grace",
			"Active accounts carry a payment-risk notice (grace / enforcement / card declined); fix billing before access is lost.",
			map[string]string{"count": itoa(grace)})
	}
	if flagged > 0 {
		r.fire(ctx, "S3:blocked", ports.LevelError,
			"Accounts blocked / suspended / paused",
			"Active-pool accounts are flagged blocked/suspended/paused upstream and cannot serve; pull or replace them.",
			map[string]string{"count": itoa(flagged)})
	}
}

// checkUnclaimedGifts (S4) is a stub: gifts are claimed fire-and-forget
// and never persisted, so there is no account_gifts table to sum value
// from. Wired so the signal slot is reserved.
func (r *Replenish) checkUnclaimedGifts(_ context.Context) {
	// TODO: once gifts are persisted, alert when unclaimed inbound gift
	// value crosses a threshold.
}

// fire sends a notification unless the same signalKey fired within the
// dedup window. On send it records the fire time.
func (r *Replenish) fire(ctx context.Context, signalKey string, level ports.NotificationLevel, title, body string, tags map[string]string) {
	r.mu.Lock()
	last, seen := r.muted[signalKey]
	if seen && r.now().Sub(last) < dedupWindow {
		r.mu.Unlock()
		return
	}
	r.muted[signalKey] = r.now()
	r.mu.Unlock()

	if err := r.Notifier.Send(ctx, ports.Notification{
		Level: level, Title: title, Body: body, Tags: tags,
	}); err != nil && r.Logger != nil {
		r.Logger.Warn("replenish notify failed",
			slog.String("signal", signalKey),
			slog.String("err", err.Error()))
	}
}

func itoa(n int) string     { return strconv.Itoa(n) }
func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }
