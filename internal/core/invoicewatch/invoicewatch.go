// Package invoicewatch is a background ticker that watches active accounts
// for unpaid (pending) invoices and auto-retries the failed auto-top-up
// charge on their behalf. It mirrors the repo's other tickers (claimer,
// refresher, replenish, failover.Recoverer): one struct, one ticker, one
// goroutine launched by main.go.
//
// Flow per tick (default cadence 15m): List(active) → fan out over accounts
// bounded by Concurrency → for each account GET /workspaces/pending-invoices;
// if the list is non-empty, POST /v2/auto-top-ups/retry-payment.
//
// Retry budgeting is in-memory (mirrors replenish's muted map): an account
// is retried at most maxRetriesPerWindow times per 24h. Once the budget is
// exhausted and the account STILL has pending invoices, the ticker stops
// retrying and fires a Warn notification instead (a paying account whose
// billing keeps failing is a "go look at this" signal). A process restart
// re-arms every account's budget, which is an acceptable trade for zero
// schema cost.
package invoicewatch

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
	defaultInterval     = 15 * time.Minute
	defaultConcurrency  = 3
	maxRetriesPerWindow = 3
	retryWindow         = 24 * time.Hour
)

// retryState tracks an account's retry budget inside the rolling window.
type retryState struct {
	count       int
	windowStart time.Time
	alerted     bool // Warn already fired for this exhausted window (dedup)
}

// Watcher scans active accounts for pending invoices and auto-retries the
// auto-top-up payment, alerting when an account's retry budget is spent.
type Watcher struct {
	Accounts    ports.AccountStore
	Upstream    *upstream.Client
	Notifier    ports.Notifier
	Logger      *slog.Logger
	Interval    time.Duration // zero selects defaultInterval
	Concurrency int           // zero selects defaultConcurrency

	mu     sync.Mutex
	states map[string]*retryState // accountID -> retry budget in the 24h window
	now    func() time.Time       // injectable clock for tests
}

// New builds a Watcher with defaults filled. Interval/Concurrency can be
// overridden by the caller after construction.
func New(accounts ports.AccountStore, up *upstream.Client, ntf ports.Notifier, logger *slog.Logger) *Watcher {
	return &Watcher{
		Accounts:    accounts,
		Upstream:    up,
		Notifier:    ntf,
		Logger:      logger,
		Interval:    defaultInterval,
		Concurrency: defaultConcurrency,
		states:      make(map[string]*retryState),
		now:         time.Now,
	}
}

// Run blocks until ctx is canceled. Intended as `go w.Run(ctx)`. A nil
// receiver or unwired dependency returns immediately so callers need not
// guard the launch.
func (w *Watcher) Run(ctx context.Context) {
	if w == nil || w.Accounts == nil || w.Upstream == nil || w.Notifier == nil {
		return
	}
	if w.Interval <= 0 {
		w.Interval = defaultInterval
	}
	if w.Concurrency <= 0 {
		w.Concurrency = defaultConcurrency
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.states == nil {
		w.states = make(map[string]*retryState)
	}
	if w.Logger != nil {
		w.Logger.Info("invoicewatch starting", slog.Duration("interval", w.Interval))
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	w.once(ctx) // immediate first pass so a pending invoice isn't stalled a full interval
	for {
		select {
		case <-ctx.Done():
			if w.Logger != nil {
				w.Logger.Info("invoicewatch stopping")
			}
			return
		case <-ticker.C:
			w.once(ctx)
		}
	}
}

// TriggerOnce runs a single scan synchronously. Used by tests and
// (optionally) an admin trigger endpoint.
func (w *Watcher) TriggerOnce(ctx context.Context) { w.once(ctx) }

// once fans out watchOne over every active account, bounded by Concurrency.
// One failing account never blocks the others.
func (w *Watcher) once(ctx context.Context) {
	if w == nil || w.Accounts == nil || w.Upstream == nil || w.Notifier == nil {
		return
	}
	accounts, err := w.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		if w.Logger != nil {
			w.Logger.Warn("invoicewatch list accounts", slog.String("err", err.Error()))
		}
		return
	}
	if len(accounts) == 0 {
		return
	}
	conc := w.Concurrency
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
			w.watchOne(ctx, &acc)
		}()
	}
	wg.Wait()
}

// watchOne fetches an account's pending invoices; if any exist it either
// retries the auto-top-up (budget permitting) or fires a Warn alert once the
// 24h retry budget is exhausted.
func (w *Watcher) watchOne(ctx context.Context, acc *domain.Account) {
	invoices, err := w.Upstream.FetchPendingInvoices(ctx, acc)
	if err != nil {
		if w.Logger != nil {
			w.Logger.Warn("invoicewatch fetch pending invoices",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	if len(invoices) == 0 {
		return // healthy — no pending invoices
	}

	if w.reserveRetry(acc.ID) {
		if err := w.Upstream.RetryAutoTopUp(ctx, acc); err != nil {
			if w.Logger != nil {
				w.Logger.Warn("invoicewatch retry auto-top-up",
					slog.String("account_id", acc.ID),
					slog.String("err", err.Error()))
			}
			return
		}
		if w.Logger != nil {
			w.Logger.Info("invoicewatch retried auto-top-up",
				slog.String("account_id", acc.ID),
				slog.Int("pending_invoices", len(invoices)))
		}
		return
	}

	// Budget exhausted and invoices still pending — alert once per window.
	w.alertExhausted(ctx, acc, len(invoices))
}

// reserveRetry returns true and consumes one unit of the account's retry
// budget if it still has budget in the current 24h window; false when the
// budget is spent. The window resets lazily on the first call after it
// elapses.
func (w *Watcher) reserveRetry(accountID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	st, ok := w.states[accountID]
	if !ok || now.Sub(st.windowStart) >= retryWindow {
		st = &retryState{windowStart: now}
		w.states[accountID] = st
	}
	if st.count >= maxRetriesPerWindow {
		return false
	}
	st.count++
	return true
}

// alertExhausted fires a Warn notification at most once per 24h window for
// an account whose retry budget is spent but still carries pending invoices.
func (w *Watcher) alertExhausted(ctx context.Context, acc *domain.Account, pending int) {
	w.mu.Lock()
	st, ok := w.states[acc.ID]
	if !ok {
		// Defensive: reserveRetry always creates state, but guard anyway.
		st = &retryState{windowStart: w.now(), count: maxRetriesPerWindow}
		w.states[acc.ID] = st
	}
	if st.alerted {
		w.mu.Unlock()
		return
	}
	st.alerted = true
	w.mu.Unlock()

	if err := w.Notifier.Send(ctx, ports.Notification{
		Level: ports.LevelWarn,
		Title: "Auto-top-up retries exhausted",
		Body:  "An active account still has pending invoices after the maximum auto-top-up retries in 24h; fix billing (card declined / outstanding balance) before access is lost.",
		Tags: map[string]string{
			"account_id":       acc.ID,
			"email":            acc.Email,
			"pending_invoices": itoa(pending),
			"retries":          itoa(maxRetriesPerWindow),
		},
	}); err != nil && w.Logger != nil {
		w.Logger.Warn("invoicewatch notify failed",
			slog.String("account_id", acc.ID),
			slog.String("err", err.Error()))
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
