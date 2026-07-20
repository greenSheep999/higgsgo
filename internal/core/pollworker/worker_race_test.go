package pollworker

// Tests for the F1 compare-and-swap guard in the pollworker terminal
// path: when TryMarkTerminal returns won=false (the sync proxy path
// won the race first), the pollworker MUST skip metering, webhook
// fire, and the in-flight decrement so we do not double-charge or
// double-notify the caller.
//
// The winning-side behaviour is already exercised by the existing
// TestWorker_ForwardsPreBalanceToMeter and TestWorker_DispatchesWebhookOnTerminal
// tests (they run with fakeJobStore.winWhen == nil, which defaults to
// won=true).

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// newWorkerWithAccounts builds a Worker with any ports.AccountStore, not
// just *fakeAccountStore. Sibling to newWorker in worker_test.go; kept
// separate so we don't have to widen the existing helper and touch every
// caller.
func newWorkerWithAccounts(t *testing.T, jobs *fakeJobStore, accs ports.AccountStore, ups *fakeUpstream) *Worker {
	t.Helper()
	return &Worker{
		Jobs:              jobs,
		Accounts:          accs,
		Upstream:          ups,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		TickInterval:      50 * time.Millisecond,
		PerJobMinInterval: 0,
		JobDeadline:       10 * time.Minute,
	}
}

// countingAccountStore extends fakeAccountStore with a counter for
// UpdateInFlight so the tests can assert the pollworker did NOT release
// the in-flight slot when it lost the CAS.
type countingAccountStore struct {
	fakeAccountStore
	inFlightDeltas atomic.Int64
	inFlightCalls  atomic.Int64
}

func (s *countingAccountStore) UpdateInFlight(_ context.Context, _ string, delta int) error {
	s.inFlightCalls.Add(1)
	s.inFlightDeltas.Add(int64(delta))
	return nil
}

// TestWorker_LostCASSkipsSideEffects models the exact F1 race: the sync
// proxy path already terminated the job before the pollworker tick fired.
// TryMarkTerminal returns won=false; the pollworker must NOT run metering,
// must NOT fire the webhook, and must NOT decrement in-flight.
func TestWorker_LostCASSkipsSideEffects(t *testing.T) {
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_lost",
				AccountID:     "acc_lost",
				APIKeyID:      "key_lost",
				ModelAlias:    "seedance-2-0-mini",
				JST:           "text2video_seedance",
				UpstreamJobID: "upst_lost",
				RequestTS:     time.Now().Add(-1 * time.Minute),
				Status:        domain.JobQueued,
				CallbackURL:   "https://x.example/lost",
				PreBalanceH:   50000,
			},
		},
		// Simulate: sync path already won this CAS. Every pollworker
		// call for this job id sees won=false.
		winWhen: func(id string, _ []domain.JobStatus, _ domain.JobStatus) bool {
			return id != "job_lost" // this job's CAS is lost
		},
	}
	accs := &countingAccountStore{fakeAccountStore: fakeAccountStore{
		acc: &domain.Account{ID: "acc_lost"},
	}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_lost", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_lost", Status: "completed", ResultURL: "https://cdn/lost.mp4"},
	}
	meter := &fakeMeter{}
	hook := &fakeWebhook{}

	w := newWorkerWithAccounts(t, jobs, accs, ups)
	w.Meter = meter
	w.Webhooks = hook

	w.tick(context.Background())

	// TryMarkTerminal was still called — that is how the pollworker
	// discovers it lost.
	if len(jobs.terminals) != 1 {
		t.Fatalf("expected 1 TryMarkTerminal call, got %d", len(jobs.terminals))
	}
	if jobs.terminals[0].won {
		t.Fatalf("test setup bug: expected won=false, got won=true")
	}

	// Metering must not have fired — the sync path already inserted the
	// usage_event. Firing again would produce a duplicate row (the
	// migration-018 UNIQUE index would catch it, but the correct
	// behaviour is not to try).
	if len(meter.calls) != 0 {
		t.Errorf("meter fired on lost CAS: got %d calls, want 0", len(meter.calls))
	}

	// Webhook must not have fired — the sync path already notified.
	if len(hook.fires) != 0 {
		t.Errorf("webhook fired on lost CAS: got %d fires, want 0", len(hook.fires))
	}

	// In-flight must not have been decremented — the sync path's
	// Store.Unlock already released the slot. Double-release would leak
	// concurrency capacity even with the MAX(0, ...) clamp because it
	// undercounts real load if two jobs terminated concurrently.
	if got := accs.inFlightCalls.Load(); got != 0 {
		t.Errorf("in-flight decrement on lost CAS: got %d UpdateInFlight calls, want 0", got)
	}
}

// TestWorker_WonCASRunsAllSideEffects is the winning-side twin — the same
// setup but with winWhen returning true, so every side effect fires
// exactly once. Catches a regression where the pollworker forgot to run
// meter/webhook/releaseInFlight even when it did win the CAS.
func TestWorker_WonCASRunsAllSideEffects(t *testing.T) {
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_won",
				AccountID:     "acc_won",
				APIKeyID:      "key_won",
				ModelAlias:    "seedance-2-0-mini",
				JST:           "text2video_seedance",
				UpstreamJobID: "upst_won",
				RequestTS:     time.Now().Add(-1 * time.Minute),
				Status:        domain.JobQueued,
				CallbackURL:   "https://x.example/won",
				PreBalanceH:   42000,
			},
		},
		// winWhen nil → defaults to won=true.
	}
	accs := &countingAccountStore{fakeAccountStore: fakeAccountStore{
		acc: &domain.Account{ID: "acc_won"},
	}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_won", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_won", Status: "completed", ResultURL: "https://cdn/won.mp4"},
	}
	meter := &fakeMeter{}
	hook := &fakeWebhook{}

	w := newWorkerWithAccounts(t, jobs, accs, ups)
	w.Meter = meter
	w.Webhooks = hook

	w.tick(context.Background())

	if len(jobs.terminals) != 1 || !jobs.terminals[0].won {
		t.Fatalf("expected 1 winning TryMarkTerminal call, got %+v", jobs.terminals)
	}
	if len(meter.calls) != 1 {
		t.Errorf("won CAS: expected 1 meter call, got %d", len(meter.calls))
	}
	if len(hook.fires) != 1 {
		t.Errorf("won CAS: expected 1 webhook fire, got %d", len(hook.fires))
	}
	// releaseInFlight should have decremented once.
	if got := accs.inFlightCalls.Load(); got != 1 {
		t.Errorf("won CAS: expected 1 UpdateInFlight call, got %d", got)
	}
	if got := accs.inFlightDeltas.Load(); got != -1 {
		t.Errorf("won CAS: expected delta=-1, got %d", got)
	}
}

// TestWorker_TryMarkTerminalUsesFromSet documents the from-status contract:
// the pollworker's CAS accepts jobs currently in queued / pending /
// in_progress and refuses to touch anything already terminal. This is the
// invariant that makes the "loser skips" branch actually get exercised in
// production — the sync path's winning CAS moves the row to a terminal
// status that is NOT in the from-set, so the pollworker's next tick sees
// won=false.
func TestWorker_TryMarkTerminalUsesFromSet(t *testing.T) {
	var captured struct {
		called bool
		from   []domain.JobStatus
	}
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_fromset",
				AccountID:     "acc_fromset",
				ModelAlias:    "seedance-2-0-mini",
				UpstreamJobID: "upst_fromset",
				RequestTS:     time.Now().Add(-30 * time.Second),
				Status:        domain.JobQueued,
			},
		},
		winWhen: func(_ string, from []domain.JobStatus, _ domain.JobStatus) bool {
			captured.called = true
			captured.from = from
			return true
		},
	}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_fromset"}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_fromset", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_fromset", Status: "completed"},
	}

	w := newWorker(t, jobs, accs, ups)
	w.tick(context.Background())

	if !captured.called {
		t.Fatalf("TryMarkTerminal was not called")
	}
	// The from-set must include the three non-terminal statuses that
	// ListPending returns. If a future refactor drops one of them the
	// pollworker will silently stop terminating jobs in that state —
	// this test catches that drift.
	got := map[domain.JobStatus]bool{}
	for _, s := range captured.from {
		got[s] = true
	}
	for _, want := range []domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning} {
		if !got[want] {
			t.Errorf("from-set missing %q — pollworker will not terminate jobs in this state", want)
		}
	}
	// And the from-set must NOT include any terminal statuses (that
	// would defeat the CAS guard against races with the sync path).
	for _, forbidden := range []domain.JobStatus{
		domain.JobCompleted, domain.JobFailed, domain.JobRefunded, domain.JobTimeout,
	} {
		if got[forbidden] {
			t.Errorf("from-set includes terminal %q — CAS guard is neutered", forbidden)
		}
	}
}

// TestWorker_UpdateStatusStillUsedForNonTerminalTransitions guards against
// an over-eager refactor that switches every jobs write to TryMarkTerminal.
// The non-terminal per-tick poll_count bump and the status-changed
// (queued → in_progress) transition must keep using UpdateStatus so the
// row still reflects live progress even before the terminal CAS runs.
func TestWorker_UpdateStatusStillUsedForNonTerminalTransitions(t *testing.T) {
	// Upstream reports in_progress — the pollworker records the status
	// transition via UpdateStatus. That row must NOT flow through the
	// CAS (which only accepts terminal statuses and would panic on
	// this input via the guard).
	jobs := &fakeJobStore{
		pending: []domain.Job{
			{
				ID:            "job_nontrm",
				AccountID:     "acc_nontrm",
				ModelAlias:    "seedance-2-0-mini",
				UpstreamJobID: "upst_nontrm",
				RequestTS:     time.Now().Add(-1 * time.Second),
				Status:        domain.JobQueued,
			},
		},
	}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_nontrm"}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_nontrm", Status: "in_progress"},
	}
	w := newWorker(t, jobs, accs, ups)
	w.tick(context.Background())

	if len(jobs.updates) == 0 {
		t.Errorf("expected UpdateStatus call for non-terminal transition, got 0")
	}
	if len(jobs.terminals) != 0 {
		t.Errorf("non-terminal path leaked into TryMarkTerminal: %+v", jobs.terminals)
	}
}

// Compile-time assertion: countingAccountStore still satisfies
// ports.AccountStore. Any interface drift shows up as a build failure.
var _ ports.AccountStore = (*countingAccountStore)(nil)
