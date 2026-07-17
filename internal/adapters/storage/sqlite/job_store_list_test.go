package sqlite

// Tests for JobStore.ListByAPIKey:
//
//   - api_key_id scoping (does not leak rows belonging to another caller)
//   - JobFilter.Status narrows the result
//   - Limit / Offset paging works
//   - Ordering is request_ts DESC (newest first)

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// insertJobForList inserts one jobs row using the fields the store cares
// about for list queries. The account row is seeded on demand so the FK
// constraint holds.
func insertJobForList(t *testing.T, s *JobStore, accountID, jobID, apiKeyID string, status domain.JobStatus, ts time.Time) {
	t.Helper()
	j := &domain.Job{
		ID:              jobID,
		APIKeyID:        apiKeyID,
		AccountID:       accountID,
		ModelAlias:      "seedance-2-0-mini",
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

func TestJobStore_ListByAPIKey_Basic(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lb")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	insertJobForList(t, store, "acc_lb", "job_a1", "key_a", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobForList(t, store, "acc_lb", "job_a2", "key_a", domain.JobFailed, now.Add(-1*time.Minute))
	insertJobForList(t, store, "acc_lb", "job_b1", "key_b", domain.JobCompleted, now)

	rows, err := store.ListByAPIKey(ctx, "key_a", ports.JobFilter{})
	if err != nil {
		t.Fatalf("list by api key: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	for _, r := range rows {
		if r.APIKeyID != "key_a" {
			t.Errorf("unexpected api_key_id: got %q want key_a", r.APIKeyID)
		}
	}
}

func TestJobStore_ListByAPIKey_StatusFilter(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_ls")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	insertJobForList(t, store, "acc_ls", "job_s1", "key_s", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobForList(t, store, "acc_ls", "job_s2", "key_s", domain.JobFailed, now.Add(-1*time.Minute))
	insertJobForList(t, store, "acc_ls", "job_s3", "key_s", domain.JobCompleted, now)

	rows, err := store.ListByAPIKey(ctx, "key_s", ports.JobFilter{Status: domain.JobCompleted})
	if err != nil {
		t.Fatalf("list by api key: %v", err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	for _, r := range rows {
		if r.Status != domain.JobCompleted {
			t.Errorf("unexpected status: got %q want completed", r.Status)
		}
	}
}

func TestJobStore_ListByAPIKey_Pagination(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lp")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	// Newest first when ordered DESC by request_ts: p3, p2, p1.
	insertJobForList(t, store, "acc_lp", "job_p1", "key_p", domain.JobCompleted, now.Add(-3*time.Minute))
	insertJobForList(t, store, "acc_lp", "job_p2", "key_p", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobForList(t, store, "acc_lp", "job_p3", "key_p", domain.JobCompleted, now.Add(-1*time.Minute))

	rows, err := store.ListByAPIKey(ctx, "key_p", ports.JobFilter{Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("list by api key: %v", err)
	}
	if got, want := len(rows), 1; got != want {
		t.Fatalf("row count: got %d want %d", got, want)
	}
	if got, want := rows[0].ID, "job_p2"; got != want {
		t.Errorf("middle row id: got %q want %q", got, want)
	}
}

func TestJobStore_ListByAPIKey_OrderByRequestTS(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lo")
	store := NewJobStore(db)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	// Insert out of chronological order to prove SQL ordering, not
	// insertion order, drives the result.
	insertJobForList(t, store, "acc_lo", "job_o2", "key_o", domain.JobCompleted, now.Add(-2*time.Minute))
	insertJobForList(t, store, "acc_lo", "job_o3", "key_o", domain.JobCompleted, now)
	insertJobForList(t, store, "acc_lo", "job_o1", "key_o", domain.JobCompleted, now.Add(-4*time.Minute))

	rows, err := store.ListByAPIKey(ctx, "key_o", ports.JobFilter{})
	if err != nil {
		t.Fatalf("list by api key: %v", err)
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
