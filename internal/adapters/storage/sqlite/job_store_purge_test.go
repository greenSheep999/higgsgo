package sqlite

// Tests for JobStore.Purge:
//
//   - Purge_ByAge: only removes rows whose finished_at is strictly older
//     than the cutoff.
//   - Purge_RespectsStatuses: rows in a status not listed in the call
//     survive the sweep even when they are old enough.
//   - Purge_NoStatuses: empty statuses slice is a no-op — an operator who
//     forgets the field must never wipe every finished job by accident.
//   - Purge_KeepsPending: pending / in-flight rows are safe from Purge
//     regardless of age, because their finished_at is NULL.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// insertJobForPurge inserts a row via the normal Create path, then (when
// finishedAt is non-zero) writes finished_at directly with a raw UPDATE so
// the test can pin the timestamp to a specific value. UpdateStatus stamps
// finished_at with time.Now(), which is too coarse for age-based sweeps.
func insertJobForPurge(t *testing.T, s *JobStore, db *DB, accountID, jobID string, status domain.JobStatus, finishedAt time.Time) {
	t.Helper()
	j := &domain.Job{
		ID:              jobID,
		AccountID:       accountID,
		ModelAlias:      "seedance-2-0-mini",
		JST:             "text2video_seedance",
		Endpoint:        "/jobs/v2/seedance_2_0",
		RequestBodyJSON: `{}`,
		RequestTS:       finishedAt,
		Status:          status,
	}
	if err := s.Create(context.Background(), j); err != nil {
		t.Fatalf("create job %s: %v", jobID, err)
	}
	if finishedAt.IsZero() {
		return
	}
	if _, err := db.ExecContext(context.Background(),
		`UPDATE jobs SET finished_at = ? WHERE id = ?`,
		fmtTime(finishedAt), jobID); err != nil {
		t.Fatalf("stamp finished_at for %s: %v", jobID, err)
	}
}

// countJobs returns the current row count for the jobs table so tests can
// assert "N rows survived the sweep" without listing every id.
func countJobs(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// TestJobStore_Purge_ByAge inserts four rows: two completed 3 days ago,
// one completed today, and one pending. The cutoff is 1 day ago with
// statuses=[completed], so only the two old completed rows must go.
func TestJobStore_Purge_ByAge(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_pa")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	threeDaysAgo := now.Add(-72 * time.Hour)
	insertJobForPurge(t, store, db, "acc_pa", "job_old1", domain.JobCompleted, threeDaysAgo)
	insertJobForPurge(t, store, db, "acc_pa", "job_old2", domain.JobCompleted, threeDaysAgo.Add(time.Minute))
	insertJobForPurge(t, store, db, "acc_pa", "job_today", domain.JobCompleted, now)
	insertJobForPurge(t, store, db, "acc_pa", "job_pending", domain.JobPending, time.Time{})

	cutoff := now.Add(-24 * time.Hour)
	n, err := store.Purge(ctx, cutoff, []domain.JobStatus{domain.JobCompleted})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Errorf("purge count: got %d want 2", n)
	}
	if got := countJobs(t, db); got != 2 {
		t.Errorf("remaining rows: got %d want 2", got)
	}
	// Sanity: the recent-completed row and the pending row must still be
	// present.
	if _, err := store.Get(ctx, "job_today"); err != nil {
		t.Errorf("recent completed row wrongly deleted: %v", err)
	}
	if _, err := store.Get(ctx, "job_pending"); err != nil {
		t.Errorf("pending row wrongly deleted: %v", err)
	}
}

// TestJobStore_Purge_RespectsStatuses seeds one old completed row and one
// old failed row, then sweeps only completed. The failed row must survive
// because its status is not in the statuses filter.
func TestJobStore_Purge_RespectsStatuses(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_ps")
	store := NewJobStore(db)
	ctx := context.Background()

	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	insertJobForPurge(t, store, db, "acc_ps", "job_c", domain.JobCompleted, old)
	insertJobForPurge(t, store, db, "acc_ps", "job_f", domain.JobFailed, old)

	// Far-future cutoff so age is not the limiter — statuses is.
	cutoff := time.Now().UTC().Add(365 * 24 * time.Hour)
	n, err := store.Purge(ctx, cutoff, []domain.JobStatus{domain.JobCompleted})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purge count: got %d want 1", n)
	}
	if _, err := store.Get(ctx, "job_f"); err != nil {
		t.Errorf("failed row wrongly deleted: %v", err)
	}
	if _, err := store.Get(ctx, "job_c"); err == nil {
		t.Errorf("completed row survived when it should have been deleted")
	}
}

// TestJobStore_Purge_NoStatuses confirms that passing an empty statuses
// slice is a no-op: nothing is deleted and the count is zero. This is the
// operator-safety guarantee — a mis-configured caller must never wipe
// every finished job by omitting the filter.
func TestJobStore_Purge_NoStatuses(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_pn")
	store := NewJobStore(db)
	ctx := context.Background()

	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	insertJobForPurge(t, store, db, "acc_pn", "job_c", domain.JobCompleted, old)
	insertJobForPurge(t, store, db, "acc_pn", "job_f", domain.JobFailed, old)

	n, err := store.Purge(ctx, time.Now().UTC().Add(365*24*time.Hour), nil)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Errorf("purge count with empty statuses: got %d want 0", n)
	}
	if got := countJobs(t, db); got != 2 {
		t.Errorf("empty-statuses purge must not touch rows: got %d want 2", got)
	}
}

// TestJobStore_Purge_KeepsPending asserts that pending / in-flight rows —
// whose finished_at is NULL by design — are never removed, no matter how
// old their request_ts is or how permissive the statuses filter looks.
func TestJobStore_Purge_KeepsPending(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_pk")
	store := NewJobStore(db)
	ctx := context.Background()

	// Insert a pending job with a request_ts far in the past; finished_at
	// stays NULL because Create never stamps it.
	old := time.Now().UTC().Add(-365 * 24 * time.Hour).Truncate(time.Second)
	insertJobForPurge(t, store, db, "acc_pk", "job_pending_old", domain.JobPending, time.Time{})
	// Also seed a running row via UpdateStatus-less shortcut — Create sets
	// status directly from the domain.Job.Status field.
	insertJobForPurge(t, store, db, "acc_pk", "job_running_old", domain.JobRunning, time.Time{})
	// Force request_ts backwards so we prove request_ts age is irrelevant
	// to Purge; only finished_at matters.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET request_ts = ? WHERE id IN ('job_pending_old','job_running_old')`,
		fmtTime(old)); err != nil {
		t.Fatalf("backdate request_ts: %v", err)
	}

	// Sweep with an aggressive filter: cutoff far in the future, every
	// status listed including pending / in-progress. Neither row should
	// disappear because both have NULL finished_at.
	cutoff := time.Now().UTC().Add(24 * time.Hour)
	n, err := store.Purge(ctx, cutoff, []domain.JobStatus{
		domain.JobPending, domain.JobRunning,
		domain.JobCompleted, domain.JobFailed,
		domain.JobRefunded, domain.JobTimeout,
	})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Errorf("purge deleted rows with NULL finished_at: got %d want 0", n)
	}
	if got := countJobs(t, db); got != 2 {
		t.Errorf("row count after purge: got %d want 2", got)
	}
}
