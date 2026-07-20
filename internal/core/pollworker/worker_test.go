package pollworker

// Tests for the pollworker Worker. Uses in-package fakes for the four
// dependency surfaces the worker touches:
//
//   - ports.JobStore    → captures UpdateStatus calls, feeds ListPending
//   - ports.AccountStore → returns a canned account from Get
//   - UpstreamPoller     → returns canned FetchStatus / FetchJob responses
//   - MeterSink          → records the preBalance arg (this is the crux of
//                          Part B: verify j.PreBalanceH flows through)
//   - WebhookSink        → records Fire(url, job) calls
//
// Only the two behaviors that Part A + Part B introduced are covered here.
// The broader pollOne surface is exercised indirectly.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// -- fakes ------------------------------------------------------------------

type fakeJobStore struct {
	mu      sync.Mutex
	pending []domain.Job
	updates []updateCall
	// terminals records every TryMarkTerminal call so tests can assert
	// on the compare-and-swap gate added by F1. The default winWhen
	// behaviour returns true so pre-F1 tests still see the pollworker
	// run its terminal side effects; tests that model the race set
	// winWhen to a stub that returns false to simulate a lost CAS.
	terminals []terminalCall
	winWhen   func(id string, from []domain.JobStatus, to domain.JobStatus) bool
}

type updateCall struct {
	id     string
	status domain.JobStatus
	meta   ports.JobMeta
}

// terminalCall captures one TryMarkTerminal invocation for assertions.
type terminalCall struct {
	id     string
	from   []domain.JobStatus
	to     domain.JobStatus
	meta   ports.JobMeta
	won    bool
}

func (s *fakeJobStore) Create(context.Context, *domain.Job) error { return nil }

func (s *fakeJobStore) UpdateStatus(_ context.Context, id string, status domain.JobStatus, meta ports.JobMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, updateCall{id: id, status: status, meta: meta})
	return nil
}

// TryMarkTerminal satisfies ports.JobStore for F1. Defaults to won=true so
// existing tests keep observing the pollworker running its terminal side
// effects. Tests that need to simulate a lost race set winWhen.
func (s *fakeJobStore) TryMarkTerminal(_ context.Context, id string, from []domain.JobStatus, to domain.JobStatus, meta ports.JobMeta) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	won := true
	if s.winWhen != nil {
		won = s.winWhen(id, from, to)
	}
	s.terminals = append(s.terminals, terminalCall{
		id: id, from: from, to: to, meta: meta, won: won,
	})
	return won, nil
}

func (s *fakeJobStore) Get(context.Context, string) (*domain.Job, error) { return nil, nil }

func (s *fakeJobStore) ListPending(context.Context) ([]domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Job, len(s.pending))
	copy(out, s.pending)
	return out, nil
}

// ListByAPIKey is a no-op here; the pollworker only exercises Create /
// UpdateStatus / ListPending. The method exists to satisfy the interface.
func (s *fakeJobStore) ListByAPIKey(context.Context, string, ports.JobFilter) ([]domain.Job, error) {
	return nil, nil
}

// ListAll is a no-op here for the same reason: pollworker never lists
// admin-scoped jobs; the method exists solely to satisfy the interface.
func (s *fakeJobStore) ListAll(context.Context, ports.JobFilter) ([]domain.Job, error) {
	return nil, nil
}

// Purge is a no-op here: the pollworker never purges jobs; the method
// exists solely to satisfy ports.JobStore so the interface stays
// implementable by the fake.
func (s *fakeJobStore) Purge(context.Context, time.Time, []domain.JobStatus) (int, error) {
	return 0, nil
}

type fakeAccountStore struct {
	acc *domain.Account
}

func (s *fakeAccountStore) Get(context.Context, string) (*domain.Account, error) {
	if s.acc == nil {
		return &domain.Account{ID: "acc_fallback"}, nil
	}
	return s.acc, nil
}

// Everything below satisfies the ports.AccountStore interface. The pollworker
// only calls Get, so the rest panic on touch to keep the test surface small.
func (s *fakeAccountStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	panic("not implemented")
}
func (s *fakeAccountStore) Upsert(context.Context, *domain.Account) error { panic("not implemented") }
func (s *fakeAccountStore) UpdateBalance(context.Context, string, int64, int64, int64) error {
	panic("not implemented")
}
func (s *fakeAccountStore) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	panic("not implemented")
}
func (s *fakeAccountStore) UpdateInFlight(context.Context, string, int) error {
	// Called by pollworker.releaseInFlight at terminal transitions
	// (ROADMAP P3-11). Tests don't need to observe the counter
	// change — real behavior is exercised by pool_group_test.go —
	// so this is a benign no-op here.
	return nil
}
func (s *fakeAccountStore) ResetAllInFlight(context.Context) (int, error) { return 0, nil }
func (s *fakeAccountStore) MarkStatus(context.Context, string, domain.AccountStatus, string) error {
	return nil
}
func (s *fakeAccountStore) MarkThrottled(context.Context, string, time.Time, string) error {
	return nil
}
func (s *fakeAccountStore) RecoverThrottled(context.Context) (int, error) { return 0, nil }
func (s *fakeAccountStore) IncrFailStreak(context.Context, string) (int, error) {
	return 0, nil
}
func (s *fakeAccountStore) ResetFailStreak(context.Context, string) error { return nil }
func (s *fakeAccountStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	panic("not implemented")
}
func (s *fakeAccountStore) Unlock(context.Context, string, string) error { panic("not implemented") }

type fakeUpstream struct {
	status *upstream.StatusResponse
	job    *upstream.FetchResponse
	err    error
}

func (u *fakeUpstream) FetchStatus(context.Context, *domain.Account, string) (*upstream.StatusResponse, error) {
	if u.err != nil {
		return nil, u.err
	}
	return u.status, nil
}

func (u *fakeUpstream) FetchJob(context.Context, *domain.Account, string) (*upstream.FetchResponse, error) {
	if u.err != nil {
		return nil, u.err
	}
	return u.job, nil
}

type fakeMeter struct {
	mu    sync.Mutex
	calls []meterCall
}

type meterCall struct {
	jobID      string
	preBalance int64
	markup     float64
}

func (m *fakeMeter) OnJobTerminal(_ context.Context, j *domain.Job, _ *domain.Account, preBalance int64, markup float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, meterCall{jobID: j.ID, preBalance: preBalance, markup: markup})
	return nil
}

type fakeWebhook struct {
	mu    sync.Mutex
	fires []webhookFire
}

type webhookFire struct {
	url   string
	jobID string
}

func (h *fakeWebhook) Fire(url string, job *domain.Job) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fires = append(h.fires, webhookFire{url: url, jobID: job.ID})
}

// -- helpers ----------------------------------------------------------------

func newWorker(t *testing.T, jobs *fakeJobStore, accs *fakeAccountStore, ups *fakeUpstream) *Worker {
	t.Helper()
	return &Worker{
		Jobs:              jobs,
		Accounts:          accs,
		Upstream:          ups,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		TickInterval:      50 * time.Millisecond,
		PerJobMinInterval: 0, // disable per-job spacing so pollOne always runs
		JobDeadline:       10 * time.Minute,
	}
}

// -- tests ------------------------------------------------------------------

// TestWorker_ForwardsPreBalanceToMeter is the core Part B assertion:
// a Job with PreBalanceH=50000 must arrive at the Meter with preBalance=50000
// (not 0), so the Recorder computes actual credits from the pre/post delta
// instead of falling back to upstream_cost.
func TestWorker_ForwardsPreBalanceToMeter(t *testing.T) {
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_pre",
				AccountID:     "acc_pre",
				ModelAlias:    "seedance-2-0-mini",
				JST:           "text2video_seedance",
				UpstreamJobID: "upst_pre",
				RequestTS:     time.Now().Add(-1 * time.Minute),
				Status:        domain.JobQueued,
				PreBalanceH:   50000,
				UpstreamCost:  700,
			},
		},
	}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_pre", SubscriptionBalance: 49300}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_pre", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_pre", Status: "completed", ResultURL: "https://cdn/out.mp4"},
	}
	meter := &fakeMeter{}

	w := newWorker(t, jobs, accs, ups)
	w.Meter = meter

	// Drive a single tick. pollOne reads the job from lstPending and calls
	// the meter after observing terminal state.
	w.tick(context.Background())

	if len(meter.calls) != 1 {
		t.Fatalf("expected 1 meter call, got %d", len(meter.calls))
	}
	got := meter.calls[0]
	if got.jobID != "job_pre" {
		t.Errorf("meter job id: got %q want %q", got.jobID, "job_pre")
	}
	if got.preBalance != 50000 {
		t.Errorf("preBalance forwarded to meter: got %d want 50000 (was 0 in the fallback path — regression)", got.preBalance)
	}
}

// TestWorker_DispatchesWebhookOnTerminal is the Part A wiring assertion:
// when a job carries CallbackURL and a WebhookSink is attached, the worker
// must invoke Fire(url, job) exactly once on the terminal transition.
func TestWorker_DispatchesWebhookOnTerminal(t *testing.T) {
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_hook",
				AccountID:     "acc_hook",
				ModelAlias:    "seedance-2-0-mini",
				JST:           "text2video_seedance",
				UpstreamJobID: "upst_hook",
				RequestTS:     time.Now().Add(-30 * time.Second),
				Status:        domain.JobQueued,
				CallbackURL:   "https://x.example/cb",
			},
		},
	}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_hook"}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_hook", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_hook", Status: "completed", ResultURL: "https://cdn/hook.mp4"},
	}
	hook := &fakeWebhook{}

	w := newWorker(t, jobs, accs, ups)
	w.Webhooks = hook

	w.tick(context.Background())

	if len(hook.fires) != 1 {
		t.Fatalf("expected 1 webhook fire, got %d", len(hook.fires))
	}
	got := hook.fires[0]
	if got.url != "https://x.example/cb" {
		t.Errorf("webhook url: got %q want %q", got.url, "https://x.example/cb")
	}
	if got.jobID != "job_hook" {
		t.Errorf("webhook job id: got %q want %q", got.jobID, "job_hook")
	}
}

// TestWorker_NoWebhookWhenCallbackEmpty guards the negative case: a job
// without a CallbackURL must NOT trigger Fire even when a Dispatcher is
// wired in. Otherwise every job would spam whatever URL the caller last
// configured, or worse, an empty URL.
func TestWorker_NoWebhookWhenCallbackEmpty(t *testing.T) {
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_nohook",
				AccountID:     "acc_nohook",
				ModelAlias:    "seedance-2-0-mini",
				JST:           "text2video_seedance",
				UpstreamJobID: "upst_nohook",
				RequestTS:     time.Now().Add(-10 * time.Second),
				Status:        domain.JobQueued,
				// CallbackURL intentionally empty.
			},
		},
	}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_nohook"}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_nohook", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_nohook", Status: "completed"},
	}
	hook := &fakeWebhook{}

	w := newWorker(t, jobs, accs, ups)
	w.Webhooks = hook

	w.tick(context.Background())

	if len(hook.fires) != 0 {
		t.Fatalf("expected 0 webhook fires for empty CallbackURL, got %d", len(hook.fires))
	}
}

// TestWorker_TimeoutFiresWebhookAndMeter exercises the deadline branch:
// a job past its deadline must transition to timeout, fire the webhook,
// and pass PreBalanceH into the Meter so accounting can still attribute
// credits upstream may have consumed before we gave up.
func TestWorker_TimeoutFiresWebhookAndMeter(t *testing.T) {
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_timeout",
				AccountID:     "acc_timeout",
				ModelAlias:    "wan_2_2_animate",
				JST:           "image2video_wan",
				UpstreamJobID: "upst_timeout",
				// Way past the 10-minute JobDeadline set in newWorker.
				RequestTS:    time.Now().Add(-2 * time.Hour),
				Status:       domain.JobRunning,
				PreBalanceH:  1200,
				UpstreamCost: 800,
				CallbackURL:  "https://x.example/timeout",
			},
		},
	}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_timeout"}}
	// Upstream is not consulted on the timeout path but must be non-nil.
	ups := &fakeUpstream{}
	meter := &fakeMeter{}
	hook := &fakeWebhook{}

	w := newWorker(t, jobs, accs, ups)
	w.Meter = meter
	w.Webhooks = hook

	w.tick(context.Background())

	// UpdateStatus is called with JobTimeout.
	if len(jobs.updates) != 1 || jobs.updates[0].status != domain.JobTimeout {
		t.Fatalf("expected 1 UpdateStatus(timeout), got %+v", jobs.updates)
	}
	// Meter receives PreBalanceH from the row.
	if len(meter.calls) != 1 || meter.calls[0].preBalance != 1200 {
		t.Errorf("timeout meter preBalance: got %+v want preBalance=1200", meter.calls)
	}
	// Webhook fires once.
	if len(hook.fires) != 1 || hook.fires[0].url != "https://x.example/timeout" {
		t.Errorf("timeout webhook: got %+v", hook.fires)
	}
}
