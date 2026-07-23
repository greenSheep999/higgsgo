// Package refresher runs a background ticker that periodically re-fetches
// each active account's balance (GET /workspaces/wallet) and entitlement
// flags (GET /user) from higgsfield's API.
//
// The account pool would otherwise drift over time:
//
//   - subscription_balance decreases as jobs run; if we never refresh, a
//     starving account keeps getting picked and every job fails with 402
//   - plan_type / has_unlim / cohort can change server-side (upgrades,
//     downgrades, sunset promos); stale flags would route paid-only jobs
//     to accounts that no longer have the entitlement
//
// The ticker is intentionally simple: per-account fan-out with a bounded
// concurrency semaphore, per-account failure isolation, and a fresh JWT
// per account (minted lazily by upstream.Client).
package refresher

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// defaultInterval is the tick cadence when Interval is left at zero.
const defaultInterval = 10 * time.Minute

// defaultConcurrency caps parallel per-account refreshes when Concurrency
// is left at zero. Kept low so a large pool does not stampede clerk.
const defaultConcurrency = 3

// Refresher periodically refreshes the balance and entitlement fields of
// every active account.
type Refresher struct {
	Accounts    ports.AccountStore
	Upstream    *upstream.Client
	Logger      *slog.Logger
	Interval    time.Duration // default 10m
	Concurrency int           // default 3

	// Failover, when non-nil, receives RecordError on every FetchWallet
	// / FetchUser / FetchUnlimActivations failure. This is the wiring
	// that turns refresher observations (session expired → 401 mint jwt)
	// into pool-level throttle / disable actions instead of a silent
	// Warn log. Nil-safe: leaving this unset preserves the pre-wiring
	// behaviour where dead accounts stay in rotation forever.
	Failover FailoverSink

	// FreeGensV2Enabled turns on the extra GET /user/free-gens/v2 fetch
	// that calibrates the free_quota columns from the authoritative
	// per-job_set_type counters. Defaults to true (set by New). When off,
	// only the flat /user *_credits fields are persisted — the pre-P1-3
	// behaviour. A v2 fetch failure is always non-fatal regardless of
	// this flag.
	FreeGensV2Enabled bool
}

// FailoverSink is the narrow subset of failover.Controller the
// refresher depends on. Defined locally to keep this package free of
// a hard dependency on internal/core/failover and to let tests pass a
// counting fake.
type FailoverSink interface {
	RecordError(ctx context.Context, accountID string, err error, bodyForRisk string)
}

// New builds a Refresher populated with sensible defaults for the caller.
// The caller must still assign Accounts, Upstream, and Logger.
func New(accounts ports.AccountStore, up *upstream.Client, logger *slog.Logger) *Refresher {
	return &Refresher{
		Accounts:          accounts,
		Upstream:          up,
		Logger:            logger,
		Interval:          defaultInterval,
		Concurrency:       defaultConcurrency,
		FreeGensV2Enabled: true,
	}
}

// Run blocks until ctx is canceled. Intended to be invoked as
// `go r.Run(ctx)` from the process boot path.
func (r *Refresher) Run(ctx context.Context) {
	if r.Interval <= 0 {
		r.Interval = defaultInterval
	}
	if r.Concurrency <= 0 {
		r.Concurrency = defaultConcurrency
	}
	r.Logger.Info("refresher starting",
		slog.Duration("interval", r.Interval),
		slog.Int("concurrency", r.Concurrency),
	)

	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	// One immediate pass so a freshly booted process does not wait an
	// entire interval before observing balances.
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			r.Logger.Info("refresher stopping")
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// TriggerOnce runs a single tick synchronously. Intended for admin
// endpoints that want to force an immediate refresh without waiting for
// the ticker interval. Errors from the underlying tick are already logged
// per-account inside tick / refreshOne, so this wrapper has nothing to
// return.
func (r *Refresher) TriggerOnce(ctx context.Context) {
	if r.Concurrency <= 0 {
		r.Concurrency = defaultConcurrency
	}
	r.tick(ctx)
}

// tick refreshes every currently-active account. A single failing account
// never blocks the others: each fan-out worker isolates errors with a
// warn-level log line.
func (r *Refresher) tick(ctx context.Context) {
	accounts, err := r.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		r.Logger.Warn("refresher list accounts", slog.String("err", err.Error()))
		return
	}
	if len(accounts) == 0 {
		return
	}
	r.Logger.Debug("refresher tick", slog.Int("accounts", len(accounts)))

	sem := make(chan struct{}, r.Concurrency)
	var wg sync.WaitGroup
	for i := range accounts {
		acc := accounts[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r.refreshOne(ctx, &acc)
		}()
	}
	wg.Wait()
}

// refreshOne fetches wallet + user for a single account and persists the
// resulting balance/entitlement updates. Failures on either endpoint are
// logged but never propagated: partial updates are always better than
// dropping the entire refresh cycle for one flaky account.
func (r *Refresher) refreshOne(ctx context.Context, acc *domain.Account) {
	// Balance. Wallet fields are already int64 hundredths — no conversion.
	wallet, walletErr := r.Upstream.FetchWallet(ctx, acc)
	if walletErr != nil {
		r.Logger.Warn("refresher fetch wallet",
			slog.String("account_id", acc.ID),
			slog.String("err", walletErr.Error()))
		// Feed the failover controller so a chain of consecutive
		// wallet-fetch failures (401 session expired / 5xx / network)
		// counts toward the consecutive-fail threshold and eventually
		// disables the account. Nil-safe when Failover is unwired.
		if r.Failover != nil {
			r.Failover.RecordError(ctx, acc.ID, walletErr, walletErr.Error())
		}
	} else {
		if err := r.Accounts.UpdateBalance(ctx, acc.ID,
			wallet.SubscriptionBalance,
			wallet.CreditsBalance,
			wallet.TotalCredits,
		); err != nil {
			r.Logger.Warn("refresher update balance",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
	}

	// Entitlements. /user reports credits as floats; convert to hundredths
	// (credits × 100) with rounding rather than truncation so 99.999 does
	// not become 9999.
	user, userErr := r.Upstream.FetchUser(ctx, acc)
	if userErr != nil {
		r.Logger.Warn("refresher fetch user",
			slog.String("account_id", acc.ID),
			slog.String("err", userErr.Error()))
		// Same failover feedback loop as wallet — /user 401s are the
		// most reliable signal that a Clerk session has expired since
		// the wallet path uses the exact same mint-jwt code path.
		if r.Failover != nil {
			r.Failover.RecordError(ctx, acc.ID, userErr, userErr.Error())
		}
		return
	}

	planEndsAt := parsePlanEndsAt(user.PlanEndsAt)
	update := ports.EntitlementUpdate{
		PlanType:           domain.PlanType(user.PlanType),
		HasUnlim:           user.HasUnlim,
		HasFlexUnlim:       user.HasFlexUnlim,
		IsProVeo3Available: user.IsProVeo3Available,
		Cohort:             user.Cohort,
		TotalPlanCredits:   int64(math.Round(user.TotalPlanCredits * 100)),
		PlanEndsAt:         planEndsAt,
	}
	if err := r.Accounts.UpdateEntitlements(ctx, acc.ID, update); err != nil {
		r.Logger.Warn("refresher update entitlements",
			slog.String("account_id", acc.ID),
			slog.String("err", err.Error()))
		return
	}

	// Free-quota counters. Written verbatim as REAL — the load-balance
	// router (prefer_free_quota) tests > 0 so fractional grants like
	// starter's qwen_camera_control_credits: 0.4 still qualify. All
	// seven columns are overwritten on every tick; a value that
	// dropped to zero server-side must reflect on disk immediately
	// (the plan no longer grants that quota).
	quota := domain.FreeQuotaCounters{
		FaceSwapCredits:          user.FaceSwapCredits,
		SoulCredits:              user.SoulCredits,
		CharacterSwapCredits:     user.CharacterSwapCredits,
		QwenCameraControlCredits: user.QwenCameraControlCredits,
		Wan25VideoCredits:        user.Wan25VideoCredits,
		Text2KeyframesCredits:    user.Text2KeyframesCredits,
		Veo3FastGenerationsCount: user.Veo3FastGenerationsCount,
	}

	// Calibrate against GET /user/free-gens/v2 when enabled. The v2 surface
	// is the authoritative per-job_set_type counter — /user reports a stale
	// soul_credits: 0 in every captured tier snapshot even though the
	// account holds hundreds of soul generations, so v2 wins where the two
	// disagree. The fetch is non-fatal: on failure we persist the /user
	// values unchanged (a warn, no failover feedback — this is an accuracy
	// enhancement, not a health signal). See reconcileFreeGensV2.
	if r.FreeGensV2Enabled {
		if v2, v2Err := r.Upstream.FetchFreeGensV2(ctx, acc); v2Err != nil {
			r.Logger.Warn("refresher fetch free-gens/v2",
				slog.String("account_id", acc.ID),
				slog.String("err", v2Err.Error()))
		} else {
			reconcileFreeGensV2(&quota, v2)
		}
	}

	if err := r.Accounts.UpdateFreeQuota(ctx, acc.ID, quota); err != nil {
		r.Logger.Warn("refresher update free quota",
			slog.String("account_id", acc.ID),
			slog.String("err", err.Error()))
	}

	// Unlim activations. GET /workspaces/unlim-activations returns
	// the authoritative list (upstream never sends deltas), so the
	// store's Replace path swaps the whole set inside a tx. A failure
	// here is non-fatal — the previous set stays in place so the
	// load-balance router can still make progress on stale data.
	//
	// Accounts on plans without any unlim bundle grant simply see an
	// empty response — the ReplaceUnlimActivations call still runs to
	// clear a stale set from a plan downgrade.
	activations, actErr := r.Upstream.FetchUnlimActivations(ctx, acc)
	if actErr != nil {
		r.Logger.Warn("refresher fetch unlim activations",
			slog.String("account_id", acc.ID),
			slog.String("err", actErr.Error()))
	} else if err := r.Accounts.ReplaceUnlimActivations(ctx, acc.ID, activations); err != nil {
		r.Logger.Warn("refresher replace unlim activations",
			slog.String("account_id", acc.ID),
			slog.String("err", err.Error()))
	}

	// Upstream-derived lifecycle signals. blocked_at / suspended_at /
	// is_paused come straight from the /user snapshot we already have;
	// is_pause_scheduled is folded into is_paused ("paused or about to
	// be" is a single alert dimension). These are informational — they
	// feed the replenish alerter, never pool gating — so a write failure
	// is non-fatal.
	if err := r.Accounts.UpdateUpstreamStatus(ctx, acc.ID, ports.UpstreamStatusUpdate{
		BlockedAt:   user.BlockedAt,
		SuspendedAt: user.SuspendedAt,
		IsPaused:    user.IsPaused || user.IsPauseScheduled,
	}); err != nil {
		r.Logger.Warn("refresher update upstream status",
			slog.String("account_id", acc.ID),
			slog.String("err", err.Error()))
	}

	// grace_status from /workspaces/notice, written separately so a
	// notice-fetch failure leaves the prior value intact rather than
	// blanking it. NormalizeNoticeStatus collapses the raw enum to a
	// payment-risk token ("" for marketing / hide notices).
	if raw, nErr := r.Upstream.FetchWorkspaceNotice(ctx, acc); nErr != nil {
		r.Logger.Warn("refresher fetch notice",
			slog.String("account_id", acc.ID),
			slog.String("err", nErr.Error()))
	} else if err := r.Accounts.UpdateGraceStatus(ctx, acc.ID, upstream.NormalizeNoticeStatus(raw)); err != nil {
		r.Logger.Warn("refresher update grace status",
			slog.String("account_id", acc.ID),
			slog.String("err", err.Error()))
	}

	r.Logger.Debug("account refreshed",
		slog.String("account_id", acc.ID),
		slog.String("plan_type", user.PlanType),
		slog.Bool("has_unlim", user.HasUnlim),
		slog.Int("unlim_activations", len(activations)),
	)
}

// freeGensV2Field maps a GET /user/free-gens/v2 job_set_type to the
// accounts free_quota column it feeds. Sourced from the free_quota_field
// assignments in data/reference/model-specs-extra.json (alias hyphens →
// job_set_type underscores). Only families that both (a) appear as a
// free-gens/v2 job_set_type and (b) have a dedicated free_quota column are
// listed — everything else is left to the flat /user value.
//
// The three soul job_set_types confirmed in the entitlements snapshots all
// collapse to soul_credits; qwen / wan2_5 are included from the same
// mapping table for forward-compatibility should upstream start returning
// them here. 待真实样本确认: only the soul trio is live-confirmed on this
// endpoint; the qwen/wan2_5 rows are grounded in the alias table, not a
// captured v2 response.
var freeGensV2Field = map[string]string{
	"text2image_soul_v2":  "soul_credits",
	"text2image_soul":     "soul_credits",
	"soul_cinematic":      "soul_credits",
	"soul_location":       "soul_credits",
	"soul_location_edit":  "soul_credits",
	"soul_cast_edit":      "soul_credits",
	"soul_cinema_studio":  "soul_credits",
	"qwen_camera_control": "qwen_camera_control_credits",
	"wan2_5_video":        "wan2_5_video_credits",
	"wan2_5_speak":        "wan2_5_video_credits",
}

// reconcileFreeGensV2 overlays the authoritative per-job_set_type counters
// from GET /user/free-gens/v2 onto the free_quota counters derived from
// GET /user. v2 is treated as the source of truth: for every column a v2
// job_set_type maps to, the column is overwritten with the v2 counter.
// When several job_set_types map to the same column (the soul family), the
// maximum counter wins — "any soul endpoint still has quota" is the signal
// the load-balance router needs. Columns no v2 item maps to keep their
// /user value untouched.
//
// A nil / empty v2 map is a no-op, so an account with no free-gens surface
// (404 → nil) or an empty items array falls back cleanly to the /user
// numbers.
func reconcileFreeGensV2(q *domain.FreeQuotaCounters, v2 map[string]float64) {
	if len(v2) == 0 {
		return
	}
	// Aggregate v2 counters per target column, taking the max across
	// every job_set_type that maps to it.
	byField := make(map[string]float64, len(v2))
	seen := make(map[string]bool, len(v2))
	for jst, counter := range v2 {
		field, ok := freeGensV2Field[jst]
		if !ok {
			continue // no dedicated column — the /user value (if any) stands
		}
		if !seen[field] || counter > byField[field] {
			byField[field] = counter
			seen[field] = true
		}
	}
	for field := range byField {
		switch field {
		case "soul_credits":
			q.SoulCredits = byField[field]
		case "qwen_camera_control_credits":
			q.QwenCameraControlCredits = byField[field]
		case "wan2_5_video_credits":
			q.Wan25VideoCredits = byField[field]
		}
	}
}

// parsePlanEndsAt accepts the RFC3339 (and RFC3339Nano) forms higgsfield
// returns and falls back to the zero value on parse failure. Zero times
// are persisted as an empty string by the SQLite adapter.
func parsePlanEndsAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
