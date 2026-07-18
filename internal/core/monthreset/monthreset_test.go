package monthreset

// Tests for the month-boundary reset ticker. An in-package fake
// APIKeyStore keeps the tests hermetic — no SQLite migrations needed —
// and only implements the two methods the ticker actually reads. Every
// other method panics so a future accidental dependency shows up
// immediately in the failing test output.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeAPIKeyStore records every List and ResetMonthlyUsage call.
// resetErr[id], when set, makes ResetMonthlyUsage return that error
// for the given key id — used by the partial-failure test.
type fakeAPIKeyStore struct {
	mu        sync.Mutex
	keys      []domain.APIKey
	listCalls int
	resetIDs  []string
	resetErr  map[string]error
}

func (s *fakeAPIKeyStore) List(context.Context) ([]domain.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	out := make([]domain.APIKey, len(s.keys))
	copy(out, s.keys)
	return out, nil
}

func (s *fakeAPIKeyStore) ResetMonthlyUsage(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.resetErr[id]; ok {
		return err
	}
	s.resetIDs = append(s.resetIDs, id)
	return nil
}

func (s *fakeAPIKeyStore) snapshot() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, len(s.resetIDs))
	copy(ids, s.resetIDs)
	return s.listCalls, ids
}

// Unused surface — every other APIKeyStore method must exist to satisfy
// the compiler but panics if a future change accidentally calls it.
func (s *fakeAPIKeyStore) Get(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) GetByHash(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) Create(context.Context, *domain.APIKey) error {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) Revoke(context.Context, string) error {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) IncrementUsage(context.Context, string, int64) error {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) ListByCPAPartner(context.Context, string) ([]domain.APIKey, error) {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) Rotate(context.Context, string) (string, error) {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) Pause(context.Context, string) error {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) Resume(context.Context, string) error {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) UpdatePlaygroundScope(context.Context, string, domain.PlaygroundScope) error {
	panic("not implemented")
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTicker_TriggerOnceResetsAllKeys(t *testing.T) {
	fake := &fakeAPIKeyStore{
		keys: []domain.APIKey{
			{ID: "key_1"},
			{ID: "key_2"},
			{ID: "key_3"},
		},
	}
	tk := &Ticker{APIKeys: fake, Logger: testLogger()}
	tk.TriggerOnce(context.Background())

	listCalls, ids := fake.snapshot()
	if listCalls != 1 {
		t.Fatalf("List calls: got %d, want 1", listCalls)
	}
	if got, want := ids, []string{"key_1", "key_2", "key_3"}; !stringSlicesEqual(got, want) {
		t.Fatalf("reset ids: got %v, want %v", got, want)
	}
}

func TestTicker_TriggerOncePartialFailure(t *testing.T) {
	boom := errors.New("db is on fire")
	fake := &fakeAPIKeyStore{
		keys: []domain.APIKey{
			{ID: "key_1"},
			{ID: "key_2"},
			{ID: "key_3"},
		},
		resetErr: map[string]error{"key_2": boom},
	}
	tk := &Ticker{APIKeys: fake, Logger: testLogger()}
	tk.TriggerOnce(context.Background())

	_, ids := fake.snapshot()
	// key_2 must be missing from the success list but the other two
	// must have been processed regardless.
	if got, want := ids, []string{"key_1", "key_3"}; !stringSlicesEqual(got, want) {
		t.Fatalf("reset ids: got %v, want %v (key_2 must be skipped)", got, want)
	}
}

func TestTicker_EmptyPool(t *testing.T) {
	fake := &fakeAPIKeyStore{}
	tk := &Ticker{APIKeys: fake, Logger: testLogger()}
	// No panic, no goroutine leak — TriggerOnce is synchronous.
	tk.TriggerOnce(context.Background())

	listCalls, ids := fake.snapshot()
	if listCalls != 1 {
		t.Fatalf("List calls: got %d, want 1", listCalls)
	}
	if len(ids) != 0 {
		t.Fatalf("reset ids: got %v, want empty", ids)
	}
}

func TestTicker_PollingModeCrossesMonthBoundary(t *testing.T) {
	fake := &fakeAPIKeyStore{
		keys: []domain.APIKey{{ID: "key_only"}},
	}

	// Clock starts fixed inside July and only moves forward when the
	// test flips 'crossed' to true. Guarded by its own mutex so the
	// ticker goroutine reading Clock() never races the test setter.
	var (
		clockMu sync.Mutex
		crossed bool
	)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		if crossed {
			return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		}
		return time.Date(2026, 7, 31, 23, 0, 0, 0, time.UTC)
	}

	tk := &Ticker{
		APIKeys:  fake,
		Logger:   testLogger(),
		Interval: 10 * time.Millisecond,
		Clock:    clock,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		tk.Run(ctx)
		close(done)
	}()

	// Give the polling loop enough time to fire several ticks; still
	// inside July, so List and ResetMonthlyUsage must both stay at 0.
	time.Sleep(80 * time.Millisecond)
	if listCalls, ids := fake.snapshot(); listCalls != 0 || len(ids) != 0 {
		t.Fatalf("pre-boundary state: List=%d resets=%v (want both zero)", listCalls, ids)
	}

	// Cross the calendar boundary. The next tick must fire exactly one
	// reset for the sole key; subsequent ticks in the same month must
	// be no-ops.
	clockMu.Lock()
	crossed = true
	clockMu.Unlock()

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if listCalls, ids := fake.snapshot(); listCalls >= 1 && len(ids) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	listCalls, ids := fake.snapshot()
	if listCalls < 1 || !stringSlicesEqual(ids, []string{"key_only"}) {
		t.Fatalf("post-boundary state: List=%d resets=%v (want List>=1 and [key_only])", listCalls, ids)
	}

	// Sleep a few more polling intervals: no further resets should
	// fire because we are still inside the same month.
	time.Sleep(60 * time.Millisecond)
	if _, ids := fake.snapshot(); len(ids) != 1 {
		t.Fatalf("second-month state: resets=%v (want no more than 1)", ids)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("ticker did not exit within 1s of ctx cancel")
	}
}

func TestTicker_RespectsCtx(t *testing.T) {
	fake := &fakeAPIKeyStore{}
	tk := &Ticker{
		APIKeys:  fake,
		Logger:   testLogger(),
		Interval: 50 * time.Millisecond,
		Clock:    func() time.Time { return time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithCancel(context.Background())

	var running atomic.Bool
	running.Store(true)
	done := make(chan struct{})
	go func() {
		tk.Run(ctx)
		running.Store(false)
		close(done)
	}()

	// Let the loop enter select at least once, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
		if running.Load() {
			t.Fatal("Run returned but running flag still true")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return within 500ms of ctx cancel")
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *fakeAPIKeyStore) UpdateMeta(context.Context, string, ports.APIKeyMetaPatch) error {
	return nil
}
