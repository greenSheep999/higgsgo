package sqlite

// Tests for the write-op surface added on top of Create / Revoke:
//
//   * Rotate      — swaps key_hash, returns a fresh plaintext, preserves
//                    every other column (name, quota, markup, CPA partner,
//                    group binding).
//   * Pause /
//     Resume     — flip status between "active" and "paused". Both are
//                    idempotent no-ops when the row is already in the
//                    target status; both refuse to touch a revoked row
//                    and surface domain.ErrAPIKeyRevoked instead.
//   * ResetMonthlyUsage — zeros monthly_used without touching quota.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// seedKey inserts a fresh api key row with sensible defaults so each test
// can focus on the transition it cares about.
func seedKey(t *testing.T, store *APIKeyStore, k *domain.APIKey) {
	t.Helper()
	if err := store.Create(context.Background(), k); err != nil {
		t.Fatalf("seed create %s: %v", k.ID, err)
	}
}

func TestAPIKeyStore_Rotate(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	// Seed with populated non-hash columns so we can verify none of them
	// are trampled by the rotation.
	orig := &domain.APIKey{
		ID:           "key_rot_1",
		KeyHash:      "hash_rot_initial",
		Name:         "rotate-me",
		CreatedBy:    "admin",
		CPAPartnerID: "partner_rot",
		GroupID:      "grp_rot",
		Status:       domain.APIKeyStatusActive,
		MonthlyQuota: 5000,
		MonthlyUsed:  120,
		MarkupPct:    1.25,
		CreatedAt:    time.Now().UTC(),
	}
	seedKey(t, store, orig)

	plaintext, err := store.Rotate(ctx, orig.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !strings.HasPrefix(plaintext, apikey.Prefix) {
		t.Errorf("plaintext missing prefix: %q", plaintext)
	}
	// The returned plaintext must hash to the row's new key_hash.
	got, err := store.Get(ctx, orig.ID)
	if err != nil {
		t.Fatalf("get after rotate: %v", err)
	}
	if got.KeyHash == orig.KeyHash {
		t.Fatalf("key_hash unchanged after rotate: %s", got.KeyHash)
	}
	if got.KeyHash != apikey.Hash(plaintext) {
		t.Errorf("key_hash mismatch: got %s want %s", got.KeyHash, apikey.Hash(plaintext))
	}
	// Every other column must be preserved.
	if got.Name != orig.Name {
		t.Errorf("Name changed: got %q want %q", got.Name, orig.Name)
	}
	if got.CPAPartnerID != orig.CPAPartnerID {
		t.Errorf("CPAPartnerID changed: got %q want %q", got.CPAPartnerID, orig.CPAPartnerID)
	}
	if got.GroupID != orig.GroupID {
		t.Errorf("GroupID changed: got %q want %q", got.GroupID, orig.GroupID)
	}
	if got.MarkupPct != orig.MarkupPct {
		t.Errorf("MarkupPct changed: got %v want %v", got.MarkupPct, orig.MarkupPct)
	}
	if got.MonthlyQuota != orig.MonthlyQuota {
		t.Errorf("MonthlyQuota changed: got %d want %d", got.MonthlyQuota, orig.MonthlyQuota)
	}
	if got.MonthlyUsed != orig.MonthlyUsed {
		t.Errorf("MonthlyUsed changed: got %d want %d", got.MonthlyUsed, orig.MonthlyUsed)
	}
	if got.Status != domain.APIKeyStatusActive {
		t.Errorf("Status changed: got %q want %q", got.Status, domain.APIKeyStatusActive)
	}
}

func TestAPIKeyStore_RotateNotFound(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	if _, err := store.Rotate(context.Background(), "does_not_exist"); !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Fatalf("rotate on unknown id: got %v want ErrAPIKeyNotFound", err)
	}
}

func TestAPIKeyStore_PauseResume(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	seedKey(t, store, &domain.APIKey{
		ID: "key_pr_1", KeyHash: "h_pr", Name: "pause-me",
		Status: domain.APIKeyStatusActive, CreatedAt: time.Now().UTC(),
	})

	if err := store.Pause(ctx, "key_pr_1"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got, err := store.Get(ctx, "key_pr_1")
	if err != nil {
		t.Fatalf("get after pause: %v", err)
	}
	if got.Status != domain.APIKeyStatusPaused {
		t.Fatalf("status after pause: got %q want %q", got.Status, domain.APIKeyStatusPaused)
	}

	// Second Pause on an already-paused row is a no-op — no error, no
	// status change.
	if err := store.Pause(ctx, "key_pr_1"); err != nil {
		t.Fatalf("pause twice: %v", err)
	}

	if err := store.Resume(ctx, "key_pr_1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, err = store.Get(ctx, "key_pr_1")
	if err != nil {
		t.Fatalf("get after resume: %v", err)
	}
	if got.Status != domain.APIKeyStatusActive {
		t.Fatalf("status after resume: got %q want %q", got.Status, domain.APIKeyStatusActive)
	}

	// Resume on an active row is a no-op.
	if err := store.Resume(ctx, "key_pr_1"); err != nil {
		t.Fatalf("resume twice: %v", err)
	}
}

func TestAPIKeyStore_PauseNotFound(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	if err := store.Pause(context.Background(), "ghost"); !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Fatalf("pause on unknown id: got %v want ErrAPIKeyNotFound", err)
	}
}

func TestAPIKeyStore_ResumeRevokedNoOp(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	seedKey(t, store, &domain.APIKey{
		ID: "key_rev_1", KeyHash: "h_rev", Name: "rev",
		Status: domain.APIKeyStatusActive, CreatedAt: time.Now().UTC(),
	})
	if err := store.Revoke(ctx, "key_rev_1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Resume on a revoked row must not silently succeed — that would
	// let an operator resurrect a compromised key. The store surfaces
	// ErrAPIKeyRevoked and leaves the row untouched.
	err := store.Resume(ctx, "key_rev_1")
	if !errors.Is(err, domain.ErrAPIKeyRevoked) {
		t.Fatalf("resume on revoked: got %v want ErrAPIKeyRevoked", err)
	}
	got, err := store.Get(ctx, "key_rev_1")
	if err != nil {
		t.Fatalf("get after resume-on-revoked: %v", err)
	}
	if got.Status != domain.APIKeyStatusRevoked {
		t.Errorf("status must stay revoked, got %q", got.Status)
	}

	// Pause on a revoked row behaves the same way.
	err = store.Pause(ctx, "key_rev_1")
	if !errors.Is(err, domain.ErrAPIKeyRevoked) {
		t.Fatalf("pause on revoked: got %v want ErrAPIKeyRevoked", err)
	}
}

func TestAPIKeyStore_ResetMonthlyUsage(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	seedKey(t, store, &domain.APIKey{
		ID: "key_ru_1", KeyHash: "h_ru", Name: "reset-me",
		Status:       domain.APIKeyStatusActive,
		MonthlyQuota: 10000,
		CreatedAt:    time.Now().UTC(),
	})
	if err := store.IncrementUsage(ctx, "key_ru_1", 500); err != nil {
		t.Fatalf("increment: %v", err)
	}
	if err := store.ResetMonthlyUsage(ctx, "key_ru_1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got, err := store.Get(ctx, "key_ru_1")
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if got.MonthlyUsed != 0 {
		t.Errorf("MonthlyUsed after reset: got %d want 0", got.MonthlyUsed)
	}
	// Quota is intentionally untouched — reset is a usage-only op.
	if got.MonthlyQuota != 10000 {
		t.Errorf("MonthlyQuota changed by reset: got %d want 10000", got.MonthlyQuota)
	}
}

func TestAPIKeyStore_ResetMonthlyUsageNotFound(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	if err := store.ResetMonthlyUsage(context.Background(), "ghost"); !errors.Is(err, domain.ErrAPIKeyNotFound) {
		t.Fatalf("reset on unknown id: got %v want ErrAPIKeyNotFound", err)
	}
}
