// Package claimer is a background ticker that auto-claims the free
// entitlements higgsfield grants to accounts but does not activate on
// their behalf: inbound gifts (GET /gifts) and platform-granted unlim
// bundles left in is_claimed=false state (GET /workspaces/unlim-activations).
//
// Both claims are idempotent fire-and-forget POSTs — the claimer sees an
// unclaimed item and claims it. It never spends money or deletes state;
// it only collects free credit/subscription the account already owns.
// Claim failures are logged, never fed to the failover controller (a
// failed freebie-grab is not an account-health signal).
//
// The component mirrors failover.Recoverer: one struct, one ticker, one
// goroutine launched by main.go — matching the repo convention of "one
// background responsibility per ticker component".
package claimer

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

const (
	defaultInterval    = 15 * time.Minute
	defaultConcurrency = 3
)

// Claimer scans active accounts and claims their unclaimed gifts and
// unlim activations on a fixed cadence.
type Claimer struct {
	Accounts    ports.AccountStore
	Upstream    *upstream.Client
	Logger      *slog.Logger
	Interval    time.Duration // zero selects defaultInterval
	Concurrency int           // zero selects defaultConcurrency
}

// New builds a Claimer with defaults filled. Interval/Concurrency can be
// overridden by the caller after construction.
func New(accounts ports.AccountStore, up *upstream.Client, logger *slog.Logger) *Claimer {
	return &Claimer{
		Accounts:    accounts,
		Upstream:    up,
		Logger:      logger,
		Interval:    defaultInterval,
		Concurrency: defaultConcurrency,
	}
}

// Run blocks until ctx is canceled. Intended as `go c.Run(ctx)`. A nil
// receiver or unwired dependency returns immediately so callers need not
// guard the launch.
func (c *Claimer) Run(ctx context.Context) {
	if c == nil || c.Accounts == nil || c.Upstream == nil {
		return
	}
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	if c.Logger != nil {
		c.Logger.Info("claimer starting", slog.Duration("interval", c.Interval))
	}
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	c.once(ctx) // immediate first pass so a pending grant isn't stalled a full interval
	for {
		select {
		case <-ctx.Done():
			if c.Logger != nil {
				c.Logger.Info("claimer stopping")
			}
			return
		case <-ticker.C:
			c.once(ctx)
		}
	}
}

// TriggerOnce runs a single claim pass synchronously. Used by tests and
// (optionally) an admin trigger endpoint.
func (c *Claimer) TriggerOnce(ctx context.Context) {
	c.once(ctx)
}

// once fans out claimOne over every active account, bounded by
// Concurrency. One failing account never blocks the others.
func (c *Claimer) once(ctx context.Context) {
	if c == nil || c.Accounts == nil || c.Upstream == nil {
		return
	}
	accounts, err := c.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("claimer list accounts", slog.String("err", err.Error()))
		}
		return
	}
	if len(accounts) == 0 {
		return
	}
	conc := c.Concurrency
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
			c.claimOne(ctx, &acc)
		}()
	}
	wg.Wait()
}

// claimOne claims one account's unclaimed gifts and unlim activations.
// Each sub-step isolates its own errors — a gifts failure never blocks
// the unlim pass and vice versa.
func (c *Claimer) claimOne(ctx context.Context, acc *domain.Account) {
	c.claimGifts(ctx, acc)
	c.claimUnlimActivations(ctx, acc)
}

// claimGifts fetches inbound unclaimed gifts and claims each. Benign
// outcomes (already claimed / active subscription) are skipped quietly.
func (c *Claimer) claimGifts(ctx context.Context, acc *domain.Account) {
	gifts, err := c.Upstream.FetchGifts(ctx, acc)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("claimer fetch gifts",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	for _, g := range gifts {
		if g.Claimed || g.ID == "" {
			continue
		}
		switch err := c.Upstream.ClaimGift(ctx, acc, g.ID); {
		case err == nil:
			if c.Logger != nil {
				c.Logger.Info("claimed gift",
					slog.String("account_id", acc.ID),
					slog.String("gift_id", g.ID),
					slog.String("plan", g.Plan))
			}
		case errors.Is(err, upstream.ErrGiftAlreadyClaimed):
			// Idempotent no-op — a prior tick or another operator got it.
		case errors.Is(err, upstream.ErrGiftActiveSubscription):
			if c.Logger != nil {
				c.Logger.Info("gift claim deferred: active subscription",
					slog.String("account_id", acc.ID),
					slog.String("gift_id", g.ID))
			}
		default:
			if c.Logger != nil {
				c.Logger.Warn("claimer claim gift",
					slog.String("account_id", acc.ID),
					slog.String("gift_id", g.ID),
					slog.String("err", err.Error()))
			}
		}
	}
}

// claimUnlimActivations fetches the account's activations and claims any
// still in is_claimed=false. Activations are deduplicated by server-side
// id: FetchUnlimActivations flattens one activation into multiple rows
// (one per unlocked endpoint), but a claim is per-activation, so we claim
// each unique id once using its first job_set_type (matching the SPA,
// which claims the whole activation once).
func (c *Claimer) claimUnlimActivations(ctx context.Context, acc *domain.Account) {
	acts, err := c.Upstream.FetchUnlimActivations(ctx, acc)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("claimer fetch unlim activations",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	seen := make(map[string]struct{})
	for _, a := range acts {
		if a.IsClaimed || a.ID == "" || a.JobSetType == "" {
			continue
		}
		if _, done := seen[a.ID]; done {
			continue
		}
		seen[a.ID] = struct{}{}
		if err := c.Upstream.ClaimUnlimActivation(ctx, acc, a.ID, a.JobSetType); err != nil {
			if c.Logger != nil {
				c.Logger.Warn("claimer claim unlim activation",
					slog.String("account_id", acc.ID),
					slog.String("activation_id", a.ID),
					slog.String("job_set_type", a.JobSetType),
					slog.String("err", err.Error()))
			}
			continue
		}
		if c.Logger != nil {
			c.Logger.Info("claimed unlim activation",
				slog.String("account_id", acc.ID),
				slog.String("activation_id", a.ID),
				slog.String("job_set_type", a.JobSetType))
		}
	}
}
