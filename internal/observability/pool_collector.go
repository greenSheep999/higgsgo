package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// DefaultPoolCollectorInterval is the tick interval when PoolCollector
// leaves Interval at the zero value.
const DefaultPoolCollectorInterval = 15 * time.Second

// PoolCollector samples the pool state on a fixed interval and pushes the
// numbers into a Metrics' gauges. Runs as its own goroutine so the HTTP
// middleware path stays free of any store lookups.
type PoolCollector struct {
	Accounts ports.AccountStore
	Jobs     ports.JobStore
	Metrics  *Metrics
	// Interval between ticks; zero uses DefaultPoolCollectorInterval.
	Interval time.Duration
	Logger   *slog.Logger
}

// Run blocks until ctx is done, sampling the pool every Interval.
// A single sample is also taken up-front so the /metrics scrape has
// meaningful values before the first tick fires.
func (c *PoolCollector) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultPoolCollectorInterval
	}
	c.tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// tick performs a single sample. Exported for tests via the test file's
// build-time visibility (same package). Errors are logged and swallowed
// so a transient store hiccup doesn't kill the goroutine.
func (c *PoolCollector) tick(ctx context.Context) {
	if c.Metrics == nil {
		return
	}
	if c.Accounts != nil {
		accts, err := c.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
		if err != nil {
			if c.Logger != nil {
				c.Logger.Warn("pool collector: list active accounts failed", slog.String("err", err.Error()))
			}
		} else {
			c.Metrics.AccountsActive.Set(float64(len(accts)))
		}
	}
	if c.Jobs != nil {
		pending, err := c.Jobs.ListPending(ctx)
		if err != nil {
			if c.Logger != nil {
				c.Logger.Warn("pool collector: list pending jobs failed", slog.String("err", err.Error()))
			}
		} else {
			c.Metrics.JobsInFlight.Set(float64(len(pending)))
		}
	}
}
