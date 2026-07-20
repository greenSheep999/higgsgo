package sqlite

// Tests for JobStore.TryMarkTerminal (F1): the compare-and-swap terminal
// transition writer. The important properties are:
//
//   1. First observer wins — the CAS moves the row into `to` and returns
//      won=true when the current status is in `from`.
//   2. Second observer loses — a follow-up CAS on the same terminal row
//      returns won=false without error and does NOT clobber meta fields
//      the winner wrote.
//   3. Under concurrent goroutines exactly one wins.
//   4. Bad inputs (empty `from`, non-terminal `to`) are refused with an
//      error rather than silently degrading to an unguarded UPDATE.
//
// These are the guarantees the sync path in core/proxy and the pollworker
// rely on to gate metering / webhook / in-flight release on a single winner.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// seedJobForCAS inserts a fresh queued jobs row so each test starts from a
// known state. Returns the store so callers can invoke methods against it.
func seedJobForCAS(t *testing.T, id, acc string) (*JobStore, *DB) {
	t.Helper()
	db := openMem(t)
	seedAccountForJob(t, db, acc)
	store := NewJobStore(db)
	if err := store.Create(context.Background(), newJob(id, acc)); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return store, db
}

func TestJobStore_TryMarkTerminal_WinnerWrites(t *testing.T) {
	store, _ := seedJobForCAS(t, "job_win", "acc_win")
	ctx := context.Background()

	won, err := store.TryMarkTerminal(ctx, "job_win",
		[]domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning},
		domain.JobCompleted,
		ports.JobMeta{
			ResultURL: "https://cdn/win.mp4",
			LatencyMS: 4200,
		},
	)
	if err != nil {
		t.Fatalf("try mark terminal (winner): %v", err)
	}
	if !won {
		t.Fatalf("winner CAS: got won=false want won=true")
	}

	got, err := store.Get(ctx, "job_win")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != domain.JobCompleted {
		t.Errorf("status: got %q want %q", got.Status, domain.JobCompleted)
	}
	if got.ResultURL != "https://cdn/win.mp4" {
		t.Errorf("result_url: got %q want %q", got.ResultURL, "https://cdn/win.mp4")
	}
	if got.LatencyMS != 4200 {
		t.Errorf("latency_ms: got %d want 4200", got.LatencyMS)
	}
	if got.FinishedAt.IsZero() {
		t.Errorf("finished_at should be stamped on terminal transition")
	}
}

func TestJobStore_TryMarkTerminal_LoserSkips(t *testing.T) {
	store, _ := seedJobForCAS(t, "job_lose", "acc_lose")
	ctx := context.Background()

	// First observer wins with meta A.
	won, err := store.TryMarkTerminal(ctx, "job_lose",
		[]domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning},
		domain.JobCompleted,
		ports.JobMeta{ResultURL: "https://cdn/winner.mp4", LatencyMS: 100},
	)
	if err != nil || !won {
		t.Fatalf("first CAS: won=%v err=%v", won, err)
	}

	// Second observer arrives after the row is already terminal. Its
	// from-set does NOT include JobCompleted, so the CAS misses. The
	// caller must see won=false with err=nil AND the row's meta must
	// remain the winner's values — even though this loser passed a
	// different ResultURL and LatencyMS.
	won2, err := store.TryMarkTerminal(ctx, "job_lose",
		[]domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning},
		domain.JobFailed,
		ports.JobMeta{ResultURL: "https://cdn/loser.mp4", LatencyMS: 999},
	)
	if err != nil {
		t.Fatalf("second CAS returned err (expected race-lost, not error): %v", err)
	}
	if won2 {
		t.Fatalf("second CAS: got won=true — race guard failed")
	}

	got, err := store.Get(ctx, "job_lose")
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != domain.JobCompleted {
		t.Errorf("status after loser: got %q want %q (winner's status)", got.Status, domain.JobCompleted)
	}
	if got.ResultURL != "https://cdn/winner.mp4" {
		t.Errorf("result_url after loser: got %q want winner's value", got.ResultURL)
	}
	if got.LatencyMS != 100 {
		t.Errorf("latency_ms after loser: got %d want 100 (winner's value)", got.LatencyMS)
	}
}

func TestJobStore_TryMarkTerminal_ConcurrentRace(t *testing.T) {
	// N goroutines all attempt the same terminal transition against the
	// same fresh row. Exactly one must win.
	//
	// modernc.org/sqlite's ":memory:" DSN gives each connection its own
	// isolated database — so a *sql.DB pool sees ephemeral, empty DBs on
	// every concurrent request. Pin the pool to a single connection so
	// every goroutine sees the same schema and row. Real production DBs
	// use a file DSN where WAL + busy_timeout let the pool span
	// connections; that path is covered indirectly by the sync +
	// pollworker integration tests.
	store, db := seedJobForCAS(t, "job_race", "acc_race")
	db.SetMaxOpenConns(1)

	const N = 8
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	errs := 0
	var errList []string

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			won, err := store.TryMarkTerminal(context.Background(), "job_race",
				[]domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning},
				domain.JobCompleted,
				ports.JobMeta{ResultURL: "https://cdn/race.mp4"},
			)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				errList = append(errList, err.Error())
				return
			}
			if won {
				wins++
			}
		}()
	}
	close(barrier)
	wg.Wait()

	if errs != 0 {
		t.Fatalf("expected 0 CAS errors, got %d (errsList=%v)", errs, errList)
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner across %d goroutines, got %d", N, wins)
	}
}

func TestJobStore_TryMarkTerminal_RejectsBadInputs(t *testing.T) {
	store, _ := seedJobForCAS(t, "job_bad", "acc_bad")
	ctx := context.Background()

	// Empty from — CAS is meaningless without a source guard.
	if _, err := store.TryMarkTerminal(ctx, "job_bad",
		nil, domain.JobCompleted, ports.JobMeta{},
	); err == nil {
		t.Errorf("empty from: expected error, got nil")
	}

	// Non-terminal target — TryMarkTerminal is not a generic UpdateStatus.
	if _, err := store.TryMarkTerminal(ctx, "job_bad",
		[]domain.JobStatus{domain.JobQueued}, domain.JobRunning, ports.JobMeta{},
	); err == nil {
		t.Errorf("non-terminal to: expected error, got nil")
	}
}

func TestJobStore_TryMarkTerminal_MissingRowIsRaceLost(t *testing.T) {
	// A CAS against a row that does not exist looks the same as a lost
	// race from the caller's perspective: won=false, err=nil. We do NOT
	// bubble up ErrJobNotFound here — see the method's doc for why.
	store, _ := seedJobForCAS(t, "job_present", "acc_present")

	won, err := store.TryMarkTerminal(context.Background(), "job_absent",
		[]domain.JobStatus{domain.JobQueued}, domain.JobCompleted,
		ports.JobMeta{},
	)
	if err != nil {
		t.Fatalf("missing row: expected nil error, got %v", err)
	}
	if won {
		t.Fatalf("missing row: expected won=false, got true")
	}
	// Ensure ErrJobNotFound was not smuggled in via a wrapped error.
	if errors.Is(err, domain.ErrJobNotFound) {
		t.Errorf("missing row: err leaked ErrJobNotFound; F1 contract says race-lost is silent")
	}
}
