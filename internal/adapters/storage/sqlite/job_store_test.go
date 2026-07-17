package sqlite

// Tests for JobStore focused on the fields added by migration
// 003_add_callback_url_and_pre_balance.sql:
//
//   - callback_url  — caller-supplied webhook URL, persisted verbatim
//   - pre_balance_h — subscription_balance snapshot used by the async
//                     metering path to compute exact credits consumed
//
// Both flow through Create and must round-trip through Get and ListPending
// so the pollworker sees the same values the /v1 handler recorded.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// seedAccountForJob inserts a minimal account row so the jobs FK constraint
// (account_id REFERENCES accounts(id)) is satisfied. The account row is a
// bare skeleton — job-level tests do not care about pool selection.
func seedAccountForJob(t *testing.T, db *DB, id string) {
	t.Helper()
	store := NewAccountStore(db)
	if err := store.Upsert(context.Background(), &domain.Account{
		ID:                  id,
		Email:               id + "@example.com",
		Password:            "-",
		SessionID:           "-",
		CookiesJSON:         "{}",
		UserAgent:           "-",
		PlanType:            domain.PlanPlus,
		SubscriptionBalance: 100000,
		Status:              domain.StatusActive,
		RegisteredAt:        time.Now().UTC(),
		ImportedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

// newJob returns a domain.Job populated with the fields the store touches.
// Callers override the fields under test.
func newJob(id, accountID string) *domain.Job {
	return &domain.Job{
		ID:              id,
		AccountID:       accountID,
		ModelAlias:      "seedance-2-0-mini",
		JST:             "text2video_seedance",
		Endpoint:        "/jobs/v2/seedance_2_0",
		RequestBodyJSON: `{}`,
		RequestTS:       time.Now().UTC(),
		UpstreamJobID:   id + "_upst",
		UpstreamCost:    1000,
		Status:          domain.JobQueued,
	}
}

// TestJobStore_CreateAndGet_PreservesCallbackAndPreBalance verifies that
// both new columns round-trip through the single-row Get path.
func TestJobStore_CreateAndGet_PreservesCallbackAndPreBalance(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_ct1")
	store := NewJobStore(db)
	ctx := context.Background()

	want := newJob("job_ct1", "acc_ct1")
	want.CallbackURL = "https://example.com/cb"
	want.PreBalanceH = 100000

	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("create job: %v", err)
	}
	got, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.CallbackURL != want.CallbackURL {
		t.Errorf("callback_url: got %q want %q", got.CallbackURL, want.CallbackURL)
	}
	if got.PreBalanceH != want.PreBalanceH {
		t.Errorf("pre_balance_h: got %d want %d", got.PreBalanceH, want.PreBalanceH)
	}
}

// TestJobStore_ListPending_IncludesNewFields verifies that both new columns
// also come back through the list path used by the pollworker's tick.
func TestJobStore_ListPending_IncludesNewFields(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_lp1")
	store := NewJobStore(db)
	ctx := context.Background()

	want := newJob("job_lp1", "acc_lp1")
	want.CallbackURL = "https://example.com/hook"
	want.PreBalanceH = 42

	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("create job: %v", err)
	}

	rows, err := store.ListPending(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 pending job, got %d", len(rows))
	}
	got := rows[0]
	if got.CallbackURL != want.CallbackURL {
		t.Errorf("callback_url (list): got %q want %q", got.CallbackURL, want.CallbackURL)
	}
	if got.PreBalanceH != want.PreBalanceH {
		t.Errorf("pre_balance_h (list): got %d want %d", got.PreBalanceH, want.PreBalanceH)
	}
}

// TestJobStore_CreateWithZeroDefaults confirms the DEFAULT ” / DEFAULT 0
// on the migration works: a Job with unset CallbackURL and PreBalanceH
// still inserts and comes back empty/zero (not NULL) via Get.
func TestJobStore_CreateWithZeroDefaults(t *testing.T) {
	db := openMem(t)
	seedAccountForJob(t, db, "acc_zd1")
	store := NewJobStore(db)
	ctx := context.Background()

	want := newJob("job_zd1", "acc_zd1") // CallbackURL and PreBalanceH stay zero-valued
	if err := store.Create(ctx, want); err != nil {
		t.Fatalf("create job: %v", err)
	}
	got, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.CallbackURL != "" {
		t.Errorf("callback_url default: got %q want empty", got.CallbackURL)
	}
	if got.PreBalanceH != 0 {
		t.Errorf("pre_balance_h default: got %d want 0", got.PreBalanceH)
	}
}
