package sqlite

// TestJobStore_TerminalIdempotency_SyncVsPollworker exercises the F1
// race directly at the storage layer: two goroutines mirroring the
// sync proxy path (core/proxy/service.go) and the pollworker
// (core/pollworker/worker.go) race to terminate the same job row.
//
// The invariants asserted match what the handlers rely on:
//
//   1. Exactly one of them wins the CAS. The loser sees won=false.
//   2. Only the winner writes to usage_events. The loser's Insert
//      lands on the migration-018 UNIQUE index and returns
//      ErrUsageEventDuplicate — treated as "already recorded" and
//      swallowed by the caller.
//   3. The final jobs row status matches the winner's terminal state.
//
// This is stronger than the per-store CAS test (concurrent goroutines
// on JobStore alone) because it wires the actual downstream
// side-effect the guard is meant to protect — the usage_events row —
// into the assertion path.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func TestJobStore_TerminalIdempotency_SyncVsPollworker(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	// Serialize connections so both goroutines see the same in-memory
	// schema + row. Production paths use file-backed WAL where the pool
	// can span connections; the F1 correctness argument does not depend
	// on which connection issues the CAS — only on the row-level
	// atomicity SQLite already provides.
	db.SetMaxOpenConns(1)

	seedAccountForJob(t, db, "acc_race_int")
	jobs := NewJobStore(db)
	events := NewUsageEventStore(db)

	const jobID = "job_race_int"
	if err := jobs.Create(ctx, newJob(jobID, "acc_race_int")); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// Both observers agree on the terminal status they would write. The
	// winner's ResultURL / LatencyMS reflect the ACTUAL terminal
	// observed; the loser's are what it OBSERVED before losing — the
	// row must end up with the winner's values regardless of which
	// goroutine wins.
	//
	// Sync path: latency = wall clock since request, ResultURL from
	// PollUntilTerminal. Pollworker: latency = wall clock since
	// RequestTS on the row, ResultURL from FetchJob. In tests we use
	// distinct payloads so we can tell them apart in the result.
	syncMeta := ports.JobMeta{ResultURL: "https://cdn/sync.mp4", LatencyMS: 1234}
	pollMeta := ports.JobMeta{ResultURL: "https://cdn/poll.mp4", LatencyMS: 5678}

	// Each goroutine wraps the CAS + usage_events Insert in the same
	// order the real handlers do: CAS first, then (only when won)
	// Insert. The Insert's UNIQUE-index guard is the belt beneath the
	// CAS's suspenders — it should never fire in this test because the
	// gate above blocks the loser first.
	type observer struct {
		name   string
		won    bool
		insErr error
	}
	runObserver := func(name string, meta ports.JobMeta) observer {
		won, err := jobs.TryMarkTerminal(ctx, jobID,
			[]domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning},
			domain.JobCompleted, meta)
		if err != nil {
			t.Errorf("[%s] CAS error: %v", name, err)
			return observer{name: name}
		}
		obs := observer{name: name, won: won}
		if won {
			// Only the winner inserts. This mirrors service.go's
			// `if won { s.Meter.OnJobTerminal(...) }` and
			// worker.go's identical gate.
			obs.insErr = events.Insert(ctx, &domain.UsageEvent{
				ID:           name + "_uev",
				HiggsgoJobID: jobID,
				GroupID:      "grp_default",
				AccountID:    "acc_race_int",
				ModelAlias:   "seedance-2-0-mini",
				JST:          "text2video_seedance",
				MediaType:    "video",
				Status:       domain.JobCompleted,
				BillingMonth: "2026-07",
				BillingDay:   "2026-07-20",
			})
		}
		return obs
	}

	// Launch both concurrently.
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]observer, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-barrier
		results[0] = runObserver("sync", syncMeta)
	}()
	go func() {
		defer wg.Done()
		<-barrier
		results[1] = runObserver("poll", pollMeta)
	}()
	close(barrier)
	wg.Wait()

	// 1. Exactly one winner.
	wins := 0
	var winner observer
	for _, r := range results {
		if r.won {
			wins++
			winner = r
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner, got %d: %+v", wins, results)
	}

	// 2. Winner's Insert succeeded. Loser did not Insert at all, so
	//    no ErrUsageEventDuplicate surfaced — that's the CAS gate
	//    doing its job. (The UNIQUE index test in
	//    usage_store_unique_test.go covers the defence-in-depth
	//    branch where the gate is bypassed.)
	if winner.insErr != nil {
		t.Fatalf("winner (%s) Insert failed: %v", winner.name, winner.insErr)
	}
	for _, r := range results {
		if !r.won && r.insErr != nil {
			t.Errorf("[%s] non-winner ran Insert (should be gated on won=true): %v",
				r.name, r.insErr)
		}
	}

	// 3. Exactly one usage_events row for this job id.
	var rowCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM usage_events WHERE higgsgo_job_id = ?`, jobID,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("usage_events rows for %s: got %d want 1", jobID, rowCount)
	}

	// 4. The jobs row status is terminal and reflects the winner's
	//    meta (ResultURL / LatencyMS). Which goroutine won is
	//    non-deterministic, so we accept either winner's payload but
	//    require the row to match ONE of them exactly.
	got, err := jobs.Get(ctx, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != domain.JobCompleted {
		t.Errorf("final status: got %q want %q", got.Status, domain.JobCompleted)
	}
	if got.ResultURL != syncMeta.ResultURL && got.ResultURL != pollMeta.ResultURL {
		t.Errorf("final result_url: got %q, want either sync (%q) or poll (%q)",
			got.ResultURL, syncMeta.ResultURL, pollMeta.ResultURL)
	}
	// The latency and result URL must come from the SAME winner —
	// TryMarkTerminal writes them in a single UPDATE, so they cannot
	// mix.
	if got.ResultURL == syncMeta.ResultURL && got.LatencyMS != syncMeta.LatencyMS {
		t.Errorf("meta split across winners: result_url=sync but latency=%d not %d",
			got.LatencyMS, syncMeta.LatencyMS)
	}
	if got.ResultURL == pollMeta.ResultURL && got.LatencyMS != pollMeta.LatencyMS {
		t.Errorf("meta split across winners: result_url=poll but latency=%d not %d",
			got.LatencyMS, pollMeta.LatencyMS)
	}
}

// TestJobStore_TerminalIdempotency_UniqueIndexCatchesGateBypass exercises
// the defence-in-depth branch: if a future refactor drops the `won` gate
// (or a code path forgets it), the UNIQUE index on
// usage_events(higgsgo_job_id) must still stop the double insert. This
// simulates that "gate regressed" scenario by having the loser attempt
// its Insert even though it lost the CAS.
func TestJobStore_TerminalIdempotency_UniqueIndexCatchesGateBypass(t *testing.T) {
	ctx := context.Background()
	db := openMem(t)
	db.SetMaxOpenConns(1)
	seedAccountForJob(t, db, "acc_bypass")
	jobs := NewJobStore(db)
	events := NewUsageEventStore(db)

	const jobID = "job_bypass"
	if err := jobs.Create(ctx, newJob(jobID, "acc_bypass")); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	// Winner CAS + Insert.
	won, err := jobs.TryMarkTerminal(ctx, jobID,
		[]domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning},
		domain.JobCompleted, ports.JobMeta{})
	if err != nil || !won {
		t.Fatalf("winner CAS: won=%v err=%v", won, err)
	}
	if err := events.Insert(ctx, &domain.UsageEvent{
		ID: "winner_uev", HiggsgoJobID: jobID, GroupID: "g", AccountID: "acc_bypass",
		ModelAlias: "m", JST: "j", MediaType: "v",
		Status: domain.JobCompleted, BillingMonth: "2026-07", BillingDay: "2026-07-20",
	}); err != nil {
		t.Fatalf("winner Insert: %v", err)
	}

	// Loser: skip the CAS gate entirely (simulating a regressed
	// caller) and try to Insert a second row for the same higgsgo_job_id.
	// The UNIQUE index must catch it as ErrUsageEventDuplicate.
	err = events.Insert(ctx, &domain.UsageEvent{
		ID: "loser_uev", HiggsgoJobID: jobID, GroupID: "g", AccountID: "acc_bypass",
		ModelAlias: "m", JST: "j", MediaType: "v",
		Status: domain.JobCompleted, BillingMonth: "2026-07", BillingDay: "2026-07-20",
	})
	if !errors.Is(err, domain.ErrUsageEventDuplicate) {
		t.Fatalf("bypassed gate: got %v, want ErrUsageEventDuplicate", err)
	}
}
