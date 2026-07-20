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
}

// New builds a Refresher populated with sensible defaults for the caller.
// The caller must still assign Accounts, Upstream, and Logger.
func New(accounts ports.AccountStore, up *upstream.Client, logger *slog.Logger) *Refresher {
	return &Refresher{
		Accounts:    accounts,
		Upstream:    up,
		Logger:      logger,
		Interval:    defaultInterval,
		Concurrency: defaultConcurrency,
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

	r.Logger.Debug("account refreshed",
		slog.String("account_id", acc.ID),
		slog.String("plan_type", user.PlanType),
		slog.Bool("has_unlim", user.HasUnlim),
		slog.Int("unlim_activations", len(activations)),
	)
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
