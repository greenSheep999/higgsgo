package replenish

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeStore embeds ports.AccountStore (nil) and overrides only the two
// methods replenish touches.
type fakeStore struct {
	ports.AccountStore
	accounts   []domain.Account
	unlimCount map[string]int
}

func (f *fakeStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	return f.accounts, nil
}
func (f *fakeStore) CountActiveUnlimByJST(context.Context) (map[string]int, error) {
	return f.unlimCount, nil
}

// fakeNotifier records sent notifications.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []ports.Notification
}

func (f *fakeNotifier) Name() string { return "fake" }
func (f *fakeNotifier) Send(_ context.Context, m ports.Notification) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}
func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}
func (f *fakeNotifier) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, m := range f.sent {
		out[i] = m.Title
	}
	return out
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mkAcct(id string, subBal int64, plansIn time.Duration) domain.Account {
	a := domain.Account{ID: id, Status: domain.StatusActive, SubscriptionBalance: subBal}
	if plansIn > 0 {
		a.PlanEndsAt = time.Now().Add(plansIn)
	}
	return a
}

func newTestReplenish(store ports.AccountStore, ntf ports.Notifier, cfg Thresholds) *Replenish {
	r := New(store, ntf, discardLogger(), cfg)
	return r
}

func TestReplenish_S2CreditExhaustion(t *testing.T) {
	// 3 of 4 accounts below floor (5000 = 50 credits) → ratio 0.75 > 0.3.
	store := &fakeStore{accounts: []domain.Account{
		mkAcct("a", 0, 0), mkAcct("b", 100, 0), mkAcct("c", 200, 0), mkAcct("d", 1_000_000, 0),
	}}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{CreditFloor: 50, CreditExhaustionPct: 0.3})
	r.TriggerOnce(context.Background())
	if ntf.count() != 1 {
		t.Fatalf("expected 1 alert, got %d: %v", ntf.count(), ntf.titles())
	}
}

func TestReplenish_S2NoAlertWhenHealthy(t *testing.T) {
	store := &fakeStore{accounts: []domain.Account{
		mkAcct("a", 1_000_000, 0), mkAcct("b", 1_000_000, 0),
	}}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{CreditFloor: 50, CreditExhaustionPct: 0.3})
	r.TriggerOnce(context.Background())
	if ntf.count() != 0 {
		t.Fatalf("expected no alert, got %v", ntf.titles())
	}
}

func TestReplenish_S5PlansEnding(t *testing.T) {
	// 2 accounts ending within 3d, threshold 1 → fire.
	store := &fakeStore{accounts: []domain.Account{
		mkAcct("a", 1_000_000, 24*time.Hour),
		mkAcct("b", 1_000_000, 48*time.Hour),
		mkAcct("c", 1_000_000, 30*24*time.Hour), // far out, ignored
	}}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{PlanEndingDays: 3, PlanEndingThreshold: 1})
	r.TriggerOnce(context.Background())
	if ntf.count() != 1 {
		t.Fatalf("expected 1 plans-ending alert, got %d: %v", ntf.count(), ntf.titles())
	}
}

func TestReplenish_S1UnlimPool(t *testing.T) {
	store := &fakeStore{
		accounts:   []domain.Account{mkAcct("a", 1_000_000, 0)},
		unlimCount: map[string]int{"seedance_2_unlimited": 1}, // below floor 3
	}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{
		MinUnlimPoolSize: 3, WatchedJobSetTypes: []string{"seedance_2_unlimited", "kling_3_unlimited"},
	})
	r.TriggerOnce(context.Background())
	// seedance below floor (1<3) AND kling absent (0<3) → 2 alerts.
	if ntf.count() != 2 {
		t.Fatalf("expected 2 unlim-pool alerts, got %d: %v", ntf.count(), ntf.titles())
	}
}

func TestReplenish_DedupWithin24h(t *testing.T) {
	store := &fakeStore{accounts: []domain.Account{mkAcct("a", 0, 0)}}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{CreditFloor: 50, CreditExhaustionPct: 0.3})
	r.TriggerOnce(context.Background())
	r.TriggerOnce(context.Background()) // same signal, within window
	if ntf.count() != 1 {
		t.Fatalf("expected dedup to 1 alert, got %d", ntf.count())
	}
}

func TestReplenish_DedupExpiresAfterWindow(t *testing.T) {
	store := &fakeStore{accounts: []domain.Account{mkAcct("a", 0, 0)}}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{CreditFloor: 50, CreditExhaustionPct: 0.3})

	base := time.Now()
	r.now = func() time.Time { return base }
	r.TriggerOnce(context.Background())
	// advance past the dedup window
	r.now = func() time.Time { return base.Add(dedupWindow + time.Minute) }
	r.TriggerOnce(context.Background())
	if ntf.count() != 2 {
		t.Fatalf("expected re-alert after window, got %d", ntf.count())
	}
}

func TestReplenish_S3GraceAndFlagged(t *testing.T) {
	a := mkAcct("a", 1_000_000, 0)
	a.GraceStatus = "grace" // payment-risk
	b := mkAcct("b", 1_000_000, 0)
	b.BlockedAt = "2026-07-20T00:00:00Z" // hard flag
	c := mkAcct("c", 1_000_000, 0)
	c.IsPaused = true // hard flag
	store := &fakeStore{accounts: []domain.Account{a, b, c, mkAcct("d", 1_000_000, 0)}}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{})
	r.TriggerOnce(context.Background())
	// One S3:grace alert (a) + one S3:blocked alert (b, c) = 2.
	if ntf.count() != 2 {
		t.Fatalf("expected 2 S3 alerts (grace + blocked), got %d: %v", ntf.count(), ntf.titles())
	}
}

func TestReplenish_S3SilentWhenClean(t *testing.T) {
	store := &fakeStore{accounts: []domain.Account{mkAcct("a", 1_000_000, 0), mkAcct("b", 1_000_000, 0)}}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{})
	r.TriggerOnce(context.Background())
	if ntf.count() != 0 {
		t.Fatalf("expected no S3 alerts on clean pool, got %v", ntf.titles())
	}
}

func TestReplenish_StubsDoNotFire(t *testing.T) {
	// No accounts, no unlim, no watched jsts → S1/S2/S5 all silent, S3/S4 stubs no-op.
	store := &fakeStore{}
	ntf := &fakeNotifier{}
	r := newTestReplenish(store, ntf, Thresholds{
		CreditFloor: 50, CreditExhaustionPct: 0.3, PlanEndingDays: 3, PlanEndingThreshold: 1, MinUnlimPoolSize: 3,
	})
	r.TriggerOnce(context.Background())
	if ntf.count() != 0 {
		t.Fatalf("expected no alerts on empty pool, got %v", ntf.titles())
	}
}
