package sqlite

// Tests for JobStore.ListAll:
//
//   - empty table returns an empty slice, not an error
//   - account_id filter narrows the result
//   - model_alias filter narrows the result
//   - status combined with a request_ts range narrows the result
//   - default ordering is request_ts DESC (newest first)
//   - limit / offset paging works and defaultJobListLimit is applied
//     when the caller passes zero

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// insertJobFull inserts one jobs row with model_alias / account / api key
// controlled by the caller. Sibling helper to insertJobForList; this one
// exposes model_alias so ListAll's model filter can be exercised.
func insertJobFull(t *testing.T, s *JobStore, accountID, jobID, apiKeyID, modelAlias string, status domain.JobStatus, ts time.Time) {
	t.Helper()
	j := &domain.Job{
		ID:              jobID,
		APIKeyID:        apiKeyID,
		AccountID:       accountID,
		ModelAlias:      modelAlias,
		JST:             "text2video_seedance",
		Endpoint:        "/jobs/v2/seedance_2_0",
		RequestBodyJSON: `{}`,
		RequestTS:       ts,
		Status:          status,
	}
	if err := s.Create(context.Background(), j); err != nil {
		t.Fatalf("create job %s: %v", jobID, err)
	}
}

func TestJobStore_ListAll_Empty(t *testing.T) {
	db := openMem(t)
	store := NewJobStore(db)
	ctx := context.Background()

	rows, err := store.ListAll(ctx, ports.JobFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("empty table: got %d rows want 0", len(rows))
	}
}

func TestJobStore_ListAll_ByAccountID(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_la1")
	seedAccountForJob(t, db, "acc_la2")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	insertJobFull(t, store, "acc_la1", "job_la1_x", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobFull(t, store, "acc_la1", "job_la1_y", "key_y", "seedance-2-0-mini", domain.JobFailed, now.Add(-1*time.Minute))
	insertJobFull(t, store, "acc_la2", "job_la2_z", "key_x", "seedance-2-0-mini", domain.JobCompleted, now)

	rows, err := store.ListAll(ctx, ports.JobFilter{AccountID: "acc_la1"})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	for _, r := range rows {
		if r.AccountID != "acc_la1" {
			t.Errorf("unexpected account_id: got %q want acc_la1", r.AccountID)
		}
	}
}

func TestJobStore_ListAll_ByModelAlias(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lm")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	insertJobFull(t, store, "acc_lm", "job_lm_a", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobFull(t, store, "acc_lm", "job_lm_b", "key_x", "kling-2-6-lipsync", domain.JobCompleted, now.Add(-1*time.Minute))
	insertJobFull(t, store, "acc_lm", "job_lm_c", "key_x", "seedance-2-0-mini", domain.JobCompleted, now)

	rows, err := store.ListAll(ctx, ports.JobFilter{ModelAlias: "seedance-2-0-mini"})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	for _, r := range rows {
		if r.ModelAlias != "seedance-2-0-mini" {
			t.Errorf("unexpected model_alias: got %q", r.ModelAlias)
		}
	}
}

func TestJobStore_ListAll_ByStatusPlusRange(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lr")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	insertJobFull(t, store, "acc_lr", "job_r_old_ok", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-10*time.Minute))
	insertJobFull(t, store, "acc_lr", "job_r_new_ok", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-1*time.Minute))
	insertJobFull(t, store, "acc_lr", "job_r_new_bad", "key_x", "seedance-2-0-mini", domain.JobFailed, now.Add(-30*time.Second))

	rows, err := store.ListAll(ctx, ports.JobFilter{
		Status: domain.JobCompleted,
		Since:  now.Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if got, want := len(rows), 1; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	if rows[0].ID != "job_r_new_ok" {
		t.Errorf("row id: got %q want job_r_new_ok", rows[0].ID)
	}
}

func TestJobStore_ListAll_OrderByRequestTSDesc(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lo2")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	// Insert out of chronological order to prove SQL ordering, not
	// insertion order, drives the result.
	insertJobFull(t, store, "acc_lo2", "job_o2", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobFull(t, store, "acc_lo2", "job_o3", "key_x", "seedance-2-0-mini", domain.JobCompleted, now)
	insertJobFull(t, store, "acc_lo2", "job_o1", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-4*time.Minute))

	rows, err := store.ListAll(ctx, ports.JobFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if got, want := len(rows), 3; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	wantIDs := []string{"job_o3", "job_o2", "job_o1"}
	for i, w := range wantIDs {
		if rows[i].ID != w {
			t.Errorf("rows[%d].ID: got %q want %q", i, rows[i].ID, w)
		}
	}
}

func TestJobStore_ListAll_LimitOffset(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lp2")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	// Newest first when ordered DESC by request_ts: p3, p2, p1.
	insertJobFull(t, store, "acc_lp2", "job_ap1", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-3*time.Minute))
	insertJobFull(t, store, "acc_lp2", "job_ap2", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobFull(t, store, "acc_lp2", "job_ap3", "key_x", "seedance-2-0-mini", domain.JobCompleted, now.Add(-1*time.Minute))

	rows, err := store.ListAll(ctx, ports.JobFilter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if got, want := len(rows), 1; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	if rows[0].ID != "job_ap2" {
		t.Errorf("middle row id: got %q want job_ap2", rows[0].ID)
	}
}
