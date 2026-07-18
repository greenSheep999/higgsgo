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
