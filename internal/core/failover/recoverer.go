package failover

import (
	"context"
	"log/slog"
	"time"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// DefaultRecoverInterval is the tick cadence for the Recoverer
// goroutine. 30 seconds is short enough that a cooled-down account
// returns to rotation without a noticeable stall, and long enough that
// the UPDATE cost stays negligible even on a large pool.
const DefaultRecoverInterval = 30 * time.Second

// Recoverer flips throttled accounts back to active once their
// throttled_until deadline passes. It is a small ticker so it can be
// launched by main.go alongside the other background goroutines.
//
// Nil-safe: a nil *Recoverer.Run is not legal Go, but a Recoverer
// constructed with a nil Accounts store returns immediately from Run
// so the callers do not need to condition their `go r.Run(ctx)` on the
// failover controller being wired.
type Recoverer struct {
	Accounts ports.AccountStore
	Logger   *slog.Logger

	// Interval overrides DefaultRecoverInterval. Zero selects the
	// default. Tests pass a short duration to drive the tick loop
	// deterministically.
	Interval time.Duration
}

// Run blocks until ctx is canceled. Intended to be invoked as
// `go r.Run(ctx)`.
func (r *Recoverer) Run(ctx context.Context) {
	if r == nil || r.Accounts == nil {
		return
	}
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultRecoverInterval
	}
	if r.Logger != nil {
		r.Logger.Info("failover recoverer starting",
			slog.Duration("interval", interval))
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.once(ctx) // one immediate pass so a stale row doesn't have to wait for the first tick
	for {
		select {
		case <-ctx.Done():
			if r.Logger != nil {
				r.Logger.Info("failover recoverer stopping")
			}
			return
		case <-ticker.C:
			r.once(ctx)
		}
	}
}

// once performs a single recovery pass. Extracted so tests can drive
// the recovery deterministically without spinning a ticker.
func (r *Recoverer) once(ctx context.Context) {
	if r == nil || r.Accounts == nil {
		return
	}
	n, err := r.Accounts.RecoverThrottled(ctx)
	if err != nil {
		if r.Logger != nil {
			r.Logger.Warn("failover recoverer: bulk flip failed",
				slog.String("err", err.Error()))
		}
		return
	}
	if n > 0 && r.Logger != nil {
		r.Logger.Info("failover recoverer: flipped throttled → active",
			slog.Int("count", n))
	}
}
