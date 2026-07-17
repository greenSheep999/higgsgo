package observability

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// stubAccountStore implements ports.AccountStore for the collector.
// Only List is used by the collector; the rest return zero values so
// unrelated callers won't accidentally exercise this in-process test.
type stubAccountStore struct {
	mu       sync.Mutex
	list     []domain.Account
	listErr  error
	listCall int
	// lastFilter records what the collector actually asked for so
	// the test can assert Status=active.
	lastFilter ports.AccountFilter
}

func (s *stubAccountStore) List(_ context.Context, f ports.AccountFilter) ([]domain.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCall++
	s.lastFilter = f
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := make([]domain.Account, len(s.list))
	copy(out, s.list)
	return out, nil
}
func (s *stubAccountStore) Get(context.Context, string) (*domain.Account, error) { return nil, nil }
func (s *stubAccountStore) Upsert(context.Context, *domain.Account) error        { return nil }
func (s *stubAccountStore) UpdateBalance(context.Context, string, int64, int64, int64) error {
	return nil
}
func (s *stubAccountStore) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	return nil
}
func (s *stubAccountStore) UpdateInFlight(context.Context, string, int) error { return nil }
func (s *stubAccountStore) MarkStatus(context.Context, string, domain.AccountStatus, string) error {
	return nil
}
func (s *stubAccountStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	return nil, "", nil
}
func (s *stubAccountStore) Unlock(context.Context, string, string) error { return nil }

// stubJobStore implements ports.JobStore. Only ListPending is used by
// the collector.
type stubJobStore struct {
	mu             sync.Mutex
	pending        []domain.Job
	pendingErr     error
	pendingCall    int
	listByAPIKeyFn func(context.Context, string, ports.JobFilter) ([]domain.Job, error)
}

func (s *stubJobStore) ListPending(context.Context) ([]domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingCall++
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	out := make([]domain.Job, len(s.pending))
	copy(out, s.pending)
	return out, nil
}
func (s *stubJobStore) Create(context.Context, *domain.Job) error { return nil }
func (s *stubJobStore) UpdateStatus(context.Context, string, domain.JobStatus, ports.JobMeta) error {
	return nil
}
func (s *stubJobStore) Get(context.Context, string) (*domain.Job, error) { return nil, nil }
func (s *stubJobStore) ListByAPIKey(ctx context.Context, k string, f ports.JobFilter) ([]domain.Job, error) {
	if s.listByAPIKeyFn != nil {
		return s.listByAPIKeyFn(ctx, k, f)
	}
	return nil, nil
}
func (s *stubJobStore) ListAll(context.Context, ports.JobFilter) ([]domain.Job, error) {
	return nil, nil
}

func TestPoolCollector_UpdatesGauges(t *testing.T) {
	accts := &stubAccountStore{
		list: []domain.Account{
			{ID: "a1", Status: domain.StatusActive},
			{ID: "a2", Status: domain.StatusActive},
			{ID: "a3", Status: domain.StatusActive},
			{ID: "a4", Status: domain.StatusActive},
			{ID: "a5", Status: domain.StatusActive},
		},
	}
	jobs := &stubJobStore{
		pending: []domain.Job{
			{ID: "j1"}, {ID: "j2"}, {ID: "j3"},
		},
	}
	m := NewMetrics()
	c := &PoolCollector{Accounts: accts, Jobs: jobs, Metrics: m}

	// Direct tick — no goroutine, no timing races.
	c.tick(context.Background())

	if got, want := gaugeValue(t, m.AccountsActive), 5.0; got != want {
		t.Errorf("AccountsActive = %v, want %v", got, want)
	}
	if got, want := gaugeValue(t, m.JobsInFlight), 3.0; got != want {
		t.Errorf("JobsInFlight = %v, want %v", got, want)
	}
	if accts.lastFilter.Status != domain.StatusActive {
		t.Errorf("List called with Status=%q, want %q", accts.lastFilter.Status, domain.StatusActive)
	}
}

func TestPoolCollector_TickSurvivesStoreError(t *testing.T) {
	accts := &stubAccountStore{listErr: errors.New("boom")}
	jobs := &stubJobStore{pendingErr: errors.New("boom")}
	m := NewMetrics()
	// Set non-zero values first so we can check they were not touched.
	m.AccountsActive.Set(42)
	m.JobsInFlight.Set(42)

	c := &PoolCollector{Accounts: accts, Jobs: jobs, Metrics: m}
	c.tick(context.Background())

	// Gauges are left as-is when the store returns an error — better a
	// stale-but-plausible reading than a spurious drop to zero.
	if got := gaugeValue(t, m.AccountsActive); got != 42 {
		t.Errorf("AccountsActive after error = %v, want 42 (unchanged)", got)
	}
	if got := gaugeValue(t, m.JobsInFlight); got != 42 {
		t.Errorf("JobsInFlight after error = %v, want 42 (unchanged)", got)
	}
}

func TestPoolCollector_RunTicksAndStops(t *testing.T) {
	accts := &stubAccountStore{
		list: []domain.Account{{ID: "a", Status: domain.StatusActive}},
	}
	jobs := &stubJobStore{}
	m := NewMetrics()
	c := &PoolCollector{
		Accounts: accts,
		Jobs:     jobs,
		Metrics:  m,
		Interval: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// Wait for the up-front tick + at least one interval-driven tick,
	// then shut the goroutine down.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		accts.mu.Lock()
		calls := accts.listCall
		accts.mu.Unlock()
		if calls >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("PoolCollector.Run did not exit after ctx cancel")
	}

	accts.mu.Lock()
	calls := accts.listCall
	accts.mu.Unlock()
	if calls < 2 {
		t.Fatalf("account List called %d times, want >= 2 (initial + tick)", calls)
	}
}

// gaugeValue reads a Gauge's current value.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatalf("gauge Write: %v", err)
	}
	if m.Gauge == nil || m.Gauge.Value == nil {
		return 0
	}
	return *m.Gauge.Value
}
