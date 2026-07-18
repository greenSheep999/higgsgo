package pollworker

// Regression test for ROADMAP P3-11: pollworker must decrement
// in_flight_jobs at every terminal transition so async jobs count
// against the group concurrency cap for their whole lifetime instead
// of only until CreateJob returns.
//
// The test wires an inflightCounterStore around the file's existing
// fakeAccountStore, drives Worker.pollOne through the three terminal
// paths (successful terminal, timeout, poll-terminal-fetch-error
// retry), and asserts the counter lands where it should.

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// inflightCounterStore embeds the file-local fakeAccountStore and
// overrides UpdateInFlight to track the counter under an atomic. Every
// other method already panics on the base fake, so a future pollworker
// change that starts touching something else fails loudly.
type inflightCounterStore struct {
	fakeAccountStore
	inflight atomic.Int32
	account  *domain.Account
}

func (s *inflightCounterStore) UpdateInFlight(_ context.Context, _ string, delta int) error {
	s.inflight.Add(int32(delta))
	return nil
}

// Get shadowing so pollOne's Accounts.Get finds our seeded row.
func (s *inflightCounterStore) Get(_ context.Context, _ string) (*domain.Account, error) {
	return s.account, nil
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestPollworker_ReleasesInFlightOnTerminalSuccess starts with an
// in-flight counter of 1 (as if the proxy's async path just handed
// off ownership), drives one poll that observes a terminal upstream
// status, and asserts the counter drops to 0.
func TestPollworker_ReleasesInFlightOnTerminalSuccess(t *testing.T) {
	accStore := &inflightCounterStore{
		account: &domain.Account{ID: "acc_a", Status: domain.StatusActive},
	}
	accStore.inflight.Store(1)

	jobStore := &fakeJobStore{}
	up := &fakeUpstream{
		status: &upstream.StatusResponse{Status: "completed"},
		job:    &upstream.FetchResponse{Status: "completed", ResultURL: "https://cdn/x.mp4"},
	}
	w := &Worker{
		Jobs:              jobStore,
		Accounts:          accStore,
		Upstream:          up,
		Logger:            testLogger(t),
		TickInterval:      time.Second,
		PerJobMinInterval: 0,
		JobDeadline:       time.Hour,
	}

	j := &domain.Job{
		ID:            "job_x",
		AccountID:     "acc_a",
		UpstreamJobID: "upstream_x",
		RequestTS:     time.Now(),
		Status:        domain.JobQueued,
	}
	w.pollOne(context.Background(), j, time.Now())

	if got := accStore.inflight.Load(); got != 0 {
		t.Errorf("terminal-success: in_flight got %d want 0", got)
	}
}

// TestPollworker_ReleasesInFlightOnTimeout drives the same worker but
// with a RequestTS old enough to trip the deadline. The counter must
// drop even though we never contacted upstream.
func TestPollworker_ReleasesInFlightOnTimeout(t *testing.T) {
	accStore := &inflightCounterStore{
		account: &domain.Account{ID: "acc_a", Status: domain.StatusActive},
	}
	accStore.inflight.Store(1)

	w := &Worker{
		Jobs:              &fakeJobStore{},
		Accounts:          accStore,
		Upstream:          &fakeUpstream{},
		Logger:            testLogger(t),
		TickInterval:      time.Second,
		PerJobMinInterval: 0,
		JobDeadline:       time.Minute,
	}

	j := &domain.Job{
		ID:            "job_slow",
		AccountID:     "acc_a",
		UpstreamJobID: "upstream_slow",
		// 2 minutes ago — safely past the 1-minute deadline.
		RequestTS: time.Now().Add(-2 * time.Minute),
		Status:    domain.JobPending,
	}
	w.pollOne(context.Background(), j, time.Now())

	if got := accStore.inflight.Load(); got != 0 {
		t.Errorf("timeout: in_flight got %d want 0", got)
	}
}

// TestPollworker_HoldsInFlightWhileFetchTerminalRetries covers the
// stall path: upstream reports terminal via FetchStatus but the
// follow-up FetchJob fails. The slot must stay reserved so we can
// retry next tick without oversubscribing the account.
func TestPollworker_HoldsInFlightWhileFetchTerminalRetries(t *testing.T) {
	accStore := &inflightCounterStore{
		account: &domain.Account{ID: "acc_a", Status: domain.StatusActive},
	}
	accStore.inflight.Store(1)

	// Two-call staged upstream: FetchStatus succeeds with terminal,
	// FetchJob fails. The existing fakeUpstream uses a single err
	// field so we build a small local wrapper.
	up := &stagedUpstream{
		statusOK: &upstream.StatusResponse{Status: "completed"},
		fetchErr: context.DeadlineExceeded,
	}
	w := &Worker{
		Jobs:              &fakeJobStore{},
		Accounts:          accStore,
		Upstream:          up,
		Logger:            testLogger(t),
		TickInterval:      time.Second,
		PerJobMinInterval: 0,
		JobDeadline:       time.Hour,
	}

	j := &domain.Job{
		ID:            "job_retry",
		AccountID:     "acc_a",
		UpstreamJobID: "upstream_retry",
		RequestTS:     time.Now(),
		Status:        domain.JobQueued,
	}
	w.pollOne(context.Background(), j, time.Now())

	if got := accStore.inflight.Load(); got != 1 {
		t.Errorf("terminal fetch stall: in_flight got %d want 1 (slot must stay reserved for retry)", got)
	}
}

// stagedUpstream splits the two upstream calls so a test can succeed
// on FetchStatus and fail on FetchJob independently. The existing
// fakeUpstream reuses one err field across both methods.
type stagedUpstream struct {
	statusOK *upstream.StatusResponse
	fetchErr error
}

func (s *stagedUpstream) FetchStatus(_ context.Context, _ *domain.Account, _ string) (*upstream.StatusResponse, error) {
	return s.statusOK, nil
}

func (s *stagedUpstream) FetchJob(_ context.Context, _ *domain.Account, _ string) (*upstream.FetchResponse, error) {
	return nil, s.fetchErr
}
