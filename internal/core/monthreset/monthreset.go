// Package monthreset runs a background ticker that zeros every API key's
// monthly_used counter at each calendar-month boundary.
//
// The api_keys table tracks monthly_quota (a hard ceiling on outbound
// spend) and monthly_used (a running counter incremented by the metering
// recorder). "Monthly" here means the calendar month in UTC, so on the
// first of every month the counter must reset — otherwise a fully-used
// key stays wedged past the intended reset date and legitimate traffic
// starts failing quota checks the moment operators expect the fresh
// budget to kick in.
//
// Two operating modes:
//
//   - Calendar mode (Interval == 0): the goroutine sleeps until the next
//     month boundary (UTC) plus a small grace window, fires one reset
//     pass, then loops. This is what production runs.
//
//   - Polling mode (Interval > 0): the goroutine wakes every Interval,
//     asks the injected Clock for "now", and only fires a reset when the
//     month has changed since the last successful pass. This mode exists
//     so tests can advance the clock deterministically without waiting
//     for a real calendar boundary.
//
// The ticker is defensive by design: one failing api_keys row must never
// abort the whole reset, so per-key failures are logged at warn and the
// loop continues. Booted from cmd/higgsgo when [tickers.month_reset]
// enabled=true.
package monthreset

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// calendarGrace pushes the calendar-mode wake up a few minutes past the
// actual boundary so any inflight metering writes on the last day of
// the month have time to settle before we zero the counters.
const calendarGrace = 5 * time.Minute

// Ticker resets api_keys.monthly_used at each month boundary.
//
// APIKeys and Logger are required. Interval and Clock are optional
// (see the package doc for their semantics). The zero value of a Ticker
// with just APIKeys+Logger set is a valid production configuration.
type Ticker struct {
	APIKeys ports.APIKeyStore
	Logger  *slog.Logger

	// Interval, when > 0, forces a check every interval instead of
	// waiting for the calendar boundary. Tests set this to speed up
	// assertions; production leaves it at 0 to use the calendar path.
	Interval time.Duration

	// Clock, when non-nil, is used to determine "current month" for
	// testing. Production leaves it nil which resolves to time.Now.
	Clock func() time.Time

	// lastResetMonth caches the (year, month) tuple of the most recent
	// successful reset pass. Only read/written from inside Run, so no
	// synchronisation is required.
	lastResetMonth monthKey
}

// monthKey identifies a calendar month in UTC. Two zero values compare
// equal, which conveniently lets the first polling tick treat "we have
// never reset" as "boundary already crossed".
type monthKey struct {
	Year  int
	Month time.Month
}

func monthOf(t time.Time) monthKey {
	u := t.UTC()
	return monthKey{Year: u.Year(), Month: u.Month()}
}

// now returns the ticker's notion of the current instant. Tests inject
// Clock to advance time deterministically.
func (t *Ticker) now() time.Time {
	if t.Clock != nil {
		return t.Clock()
	}
	return time.Now()
}

// Run blocks until ctx is canceled. Intended to be invoked as
// `go t.Run(ctx)` from the process boot path.
func (t *Ticker) Run(ctx context.Context) {
	if t.Interval > 0 {
		t.runPolling(ctx)
		return
	}
	t.runCalendar(ctx)
}

// runCalendar sleeps until the next month boundary + grace window,
// fires a reset, then loops. Production path.
func (t *Ticker) runCalendar(ctx context.Context) {
	t.Logger.Info("month reset ticker starting", slog.String("mode", "calendar"))
	for {
		wake := nextMonthBoundary(t.now()).Add(calendarGrace)
		wait := time.Until(wake)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Logger.Info("month reset ticker stopping")
			return
		case <-timer.C:
			t.resetAll(ctx)
			t.lastResetMonth = monthOf(t.now())
		}
	}
}

// runPolling wakes every Interval, consults Clock, and only fires a
// reset when the calendar month has changed since the last successful
// pass. Test path.
func (t *Ticker) runPolling(ctx context.Context) {
	t.Logger.Info("month reset ticker starting",
		slog.String("mode", "polling"),
		slog.Duration("interval", t.Interval),
	)
	// Seed lastResetMonth with "now" so an immediate first tick
	// inside the same month does not spuriously reset a freshly
	// booted process. Tests that want the first tick to fire simply
	// advance Clock past the current month before the goroutine runs.
	t.lastResetMonth = monthOf(t.now())
	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Logger.Info("month reset ticker stopping")
			return
		case <-ticker.C:
			cur := monthOf(t.now())
			if cur == t.lastResetMonth {
				continue
			}
			t.resetAll(ctx)
			t.lastResetMonth = cur
		}
	}
}

// TriggerOnce runs a single reset pass synchronously. Intended for an
// admin endpoint or tests that want to force a reset without waiting
// for the month boundary. Errors from the underlying pass are already
// logged per key inside resetAll, so this wrapper has nothing to return.
func (t *Ticker) TriggerOnce(ctx context.Context) {
	t.resetAll(ctx)
}

// resetAll fetches every api key and zeros its monthly_used counter.
// A single failing row never aborts the pass: per-key errors are
// logged at warn and the loop continues.
func (t *Ticker) resetAll(ctx context.Context) {
	keys, err := t.APIKeys.List(ctx)
	if err != nil {
		t.Logger.Warn("month reset list keys", slog.String("err", err.Error()))
		return
	}
	if len(keys) == 0 {
		t.Logger.Info("month reset done, no keys to process")
		return
	}
	var (
		mu     sync.Mutex
		failed int
	)
	for i := range keys {
		id := keys[i].ID
		if err := t.APIKeys.ResetMonthlyUsage(ctx, id); err != nil {
			mu.Lock()
			failed++
			mu.Unlock()
			t.Logger.Warn("month reset key",
				slog.String("api_key_id", id),
				slog.String("err", err.Error()))
			continue
		}
	}
	t.Logger.Info("month reset done",
		slog.Int("keys", len(keys)),
		slog.Int("failed", failed),
	)
}

// nextMonthBoundary returns the first instant of the calendar month
// after t, in UTC. Used by runCalendar to decide when to wake.
func nextMonthBoundary(t time.Time) time.Time {
	u := t.UTC()
	// time.Date normalises overflow: month=13 becomes year+1, month=1.
	return time.Date(u.Year(), u.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
