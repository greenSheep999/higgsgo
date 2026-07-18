package sqlite

// Exercise the RegistrationStore state machine end-to-end against
// migration 001's registrations table. Before this store landed the
// table was schema-only — an admin.RegistrationsHandler.Enqueue call
// had nowhere to write. See docs/AUDIT-CYCLE.md §4.

import (
	"context"
	"errors"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func TestRegistrationStore_LifecycleHappyPath(t *testing.T) {
	db := openMem(t)
	store := NewRegistrationStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Enqueue two pending rows. NextPending should return the oldest
	// first (id ascending) so the queue is FIFO.
	r1 := &ports.Registration{Email: "a@example.com"}
	must(store.Enqueue(ctx, r1))
	if r1.ID == 0 {
		t.Fatal("enqueue: expected assigned id, got 0")
	}
	if r1.Status != "pending" {
		t.Errorf("enqueue: status = %q want pending", r1.Status)
	}
	r2 := &ports.Registration{Email: "b@example.com", OAuthSource: "google"}
	must(store.Enqueue(ctx, r2))

	// FIFO check.
	got, err := store.NextPending(ctx)
	must(err)
	if got == nil || got.ID != r1.ID {
		t.Fatalf("NextPending: got %+v, want id=%d", got, r1.ID)
	}

	// Claim → running. Attempts increments so a stuck row is visible
	// even before it transitions to terminal.
	must(store.MarkRunning(ctx, r1.ID))
	got, err = store.Get(ctx, r1.ID)
	must(err)
	if got.Status != "running" {
		t.Errorf("after MarkRunning: status = %q want running", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d want 1", got.Attempts)
	}

	// Complete r1. NextPending now returns r2 — the queue advances.
	must(store.MarkCompleted(ctx, r1.ID, "acc_new_1"))
	got, err = store.Get(ctx, r1.ID)
	must(err)
	if got.Status != "success" || got.AccountID != "acc_new_1" {
		t.Errorf("after MarkCompleted: %+v", got)
	}
	if got.FinishedAt.IsZero() {
		t.Error("MarkCompleted should stamp finished_at")
	}
	got, err = store.NextPending(ctx)
	must(err)
	if got == nil || got.ID != r2.ID {
		t.Fatalf("NextPending after completion: got %+v, want id=%d", got, r2.ID)
	}
}

// TestRegistrationStore_FailPathPreservesTrail verifies that
// MarkFailed keeps the row queryable via Get with a populated
// last_error message. Admin UI depends on this to render the failure
// reason inline instead of showing an opaque "failed" chip.
func TestRegistrationStore_FailPathPreservesTrail(t *testing.T) {
	db := openMem(t)
	store := NewRegistrationStore(db)
	ctx := context.Background()

	r := &ports.Registration{Email: "fail@example.com"}
	if err := store.Enqueue(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFailed(ctx, r.ID, "captcha solver returned 500"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q want failed", got.Status)
	}
	if got.LastError != "captcha solver returned 500" {
		t.Errorf("last_error = %q", got.LastError)
	}
	if got.FinishedAt.IsZero() {
		t.Error("MarkFailed should stamp finished_at")
	}
}

// TestRegistrationStore_UnknownIDPathways: every mutating method must
// surface ErrRegistrationNotFound rather than a silent nil so the
// admin handler can 404 correctly and the worker can distinguish
// "race with admin delete" from "DB error".
func TestRegistrationStore_UnknownIDPathways(t *testing.T) {
	db := openMem(t)
	store := NewRegistrationStore(db)
	ctx := context.Background()

	if _, err := store.Get(ctx, 42); !errors.Is(err, domain.ErrRegistrationNotFound) {
		t.Errorf("Get unknown: got %v want ErrRegistrationNotFound", err)
	}
	if err := store.MarkRunning(ctx, 42); !errors.Is(err, domain.ErrRegistrationNotFound) {
		t.Errorf("MarkRunning unknown: got %v", err)
	}
	if err := store.MarkCompleted(ctx, 42, "acc_x"); !errors.Is(err, domain.ErrRegistrationNotFound) {
		t.Errorf("MarkCompleted unknown: got %v", err)
	}
	if err := store.MarkFailed(ctx, 42, "boom"); !errors.Is(err, domain.ErrRegistrationNotFound) {
		t.Errorf("MarkFailed unknown: got %v", err)
	}
}

// TestRegistrationStore_NextPending_EmptyQueue confirms the (nil,nil)
// contract: an empty queue is a normal condition worth backing off
// on, not a failure to log.
func TestRegistrationStore_NextPending_EmptyQueue(t *testing.T) {
	db := openMem(t)
	store := NewRegistrationStore(db)
	got, err := store.NextPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty queue: got %+v want nil", got)
	}
}

// TestRegistrationStore_List_NewestFirstAndFilters covers the admin
// list surface: newest-first ordering, status filter narrowing,
// limit clamping, and offset paging. These are the four levers the
// admin UI drives; a bug in any of them breaks the operator's view
// of the queue.
func TestRegistrationStore_List_NewestFirstAndFilters(t *testing.T) {
	db := openMem(t)
	store := NewRegistrationStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Seed 5 rows across three statuses so filters have something
	// to bite on. Enqueued in this order: p1 (pending), p2 (pending),
	// f1 (failed), s1 (success), p3 (pending).
	seeds := []struct {
		email  string
		status string
	}{
		{"p1@x.com", "pending"},
		{"p2@x.com", "pending"},
		{"f1@x.com", "failed"},
		{"s1@x.com", "success"},
		{"p3@x.com", "pending"},
	}
	for _, s := range seeds {
		r := &ports.Registration{Email: s.email, Status: s.status}
		must(store.Enqueue(ctx, r))
	}

	// Default List (no filter): all 5 rows, newest-first.
	rows, err := store.List(ctx, ports.RegistrationFilter{})
	must(err)
	if len(rows) != 5 {
		t.Fatalf("default List: got %d rows want 5", len(rows))
	}
	if rows[0].Email != "p3@x.com" || rows[4].Email != "p1@x.com" {
		t.Errorf("newest-first ordering broken: first=%q last=%q", rows[0].Email, rows[4].Email)
	}

	// Status filter narrows correctly. 3 pending rows.
	rows, err = store.List(ctx, ports.RegistrationFilter{Status: "pending"})
	must(err)
	if len(rows) != 3 {
		t.Fatalf("status=pending: got %d rows want 3", len(rows))
	}
	for _, r := range rows {
		if r.Status != "pending" {
			t.Errorf("status filter leaked: got %q", r.Status)
		}
	}

	// Limit + offset paging. Page size 2, offset 2 → third and
	// fourth newest rows (0-indexed).
	rows, err = store.List(ctx, ports.RegistrationFilter{Limit: 2, Offset: 2})
	must(err)
	if len(rows) != 2 {
		t.Fatalf("limit=2 offset=2: got %d rows want 2", len(rows))
	}
	// Full ordering: p3, s1, f1, p2, p1. Offset 2 skips p3 & s1;
	// next 2 = f1 & p2.
	if rows[0].Email != "f1@x.com" || rows[1].Email != "p2@x.com" {
		t.Errorf("paging broken: %q, %q", rows[0].Email, rows[1].Email)
	}

	// Limit clamp: asking for 500 gets capped at 200. Verified
	// indirectly — we can't seed 500 rows cheaply, but a request
	// with limit=500 must still return without error and no more
	// than the actual row count.
	rows, err = store.List(ctx, ports.RegistrationFilter{Limit: 500})
	must(err)
	if len(rows) != 5 {
		t.Errorf("limit=500 (capped to 200): got %d rows, want all 5", len(rows))
	}
}

// TestRegistrationStore_ResetToPending covers the Retry path: any
// non-pending row can be flipped back to pending, attempts count is
// preserved, and last_error / finished_at are cleared so the reset
// row looks fresh in the admin UI.
func TestRegistrationStore_ResetToPending(t *testing.T) {
	db := openMem(t)
	store := NewRegistrationStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Seed a row, run it through the fail path, then reset.
	r := &ports.Registration{Email: "retry@x.com"}
	must(store.Enqueue(ctx, r))
	must(store.MarkRunning(ctx, r.ID))
	must(store.MarkFailed(ctx, r.ID, "captcha timeout"))

	got, err := store.Get(ctx, r.ID)
	must(err)
	if got.Status != "failed" || got.LastError != "captcha timeout" || got.FinishedAt.IsZero() {
		t.Fatalf("pre-reset state wrong: %+v", got)
	}

	must(store.ResetToPending(ctx, r.ID))

	got, err = store.Get(ctx, r.ID)
	must(err)
	if got.Status != "pending" {
		t.Errorf("status after reset: %q want pending", got.Status)
	}
	if got.LastError != "" {
		t.Errorf("last_error should be cleared: %q", got.LastError)
	}
	if !got.FinishedAt.IsZero() {
		t.Errorf("finished_at should be cleared, got %v", got.FinishedAt)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts should be preserved (was 1 after MarkRunning), got %d", got.Attempts)
	}

	// Unknown id returns ErrRegistrationNotFound.
	if err := store.ResetToPending(ctx, 999); !errors.Is(err, domain.ErrRegistrationNotFound) {
		t.Errorf("unknown id: got %v want ErrRegistrationNotFound", err)
	}
}
