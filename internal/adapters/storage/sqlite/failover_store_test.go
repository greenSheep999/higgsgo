package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// seedAccount inserts a minimal accounts row so foreign-key constraints
// on account_failover_events pass.
func seedAccount(t *testing.T, store *AccountStore, id string) {
	t.Helper()
	err := store.Upsert(context.Background(), &domain.Account{
		ID:           id,
		Email:        id + "@example.com",
		Password:     "x",
		SessionID:    "sess",
		CookiesJSON:  "{}",
		UserAgent:    "ua",
		PlanType:     domain.PlanPro,
		Status:       domain.StatusActive,
		RegisteredAt: time.Now().UTC(),
		ImportedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed account %s: %v", id, err)
	}
}

func TestFailoverEventStore_InsertCountList(t *testing.T) {
	db := openMem(t)
	accts := NewAccountStore(db)
	seedAccount(t, accts, "acc-1")
	seedAccount(t, accts, "acc-2")

	evs := NewFailoverEventStore(db)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := evs.Insert(ctx, "acc-1", ports.FailoverEventFailure, "consec_fail", 401); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := evs.Insert(ctx, "acc-2", ports.FailoverEventFailure, "consec_fail", 401); err != nil {
		t.Fatal(err)
	}

	n, err := evs.Count(ctx, "acc-1", ports.FailoverEventFailure, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("count(acc-1): got %d want 5", n)
	}
	// Different kind => 0.
	if n, _ := evs.Count(ctx, "acc-1", ports.FailoverEventThrottle, 3600); n != 0 {
		t.Errorf("throttle kind count: got %d want 0", n)
	}
	// Window <= 0 short-circuits.
	if n, _ := evs.Count(ctx, "acc-1", ports.FailoverEventFailure, 0); n != 0 {
		t.Errorf("windowSec=0 count: got %d want 0", n)
	}

	rows, err := evs.List(ctx, "acc-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Errorf("List(acc-1) len: got %d want 5", len(rows))
	}
	for _, r := range rows {
		if r.AccountID != "acc-1" {
			t.Errorf("List returned account %s in acc-1 query", r.AccountID)
		}
	}

	// CountRecentDisables aggregates by reason ("consec_fail" / "evict")
	// across accounts; acc-1 + acc-2 both have consec_fail reasons so it
	// should return 2.
	if n, _ := evs.CountRecentDisables(ctx, 3600); n != 2 {
		t.Errorf("CountRecentDisables: got %d want 2", n)
	}

	// DeleteForAccount wipes the target only.
	if err := evs.DeleteForAccount(ctx, "acc-1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := evs.Count(ctx, "acc-1", ports.FailoverEventFailure, 3600); n != 0 {
		t.Errorf("after delete acc-1 count: got %d want 0", n)
	}
	if n, _ := evs.Count(ctx, "acc-2", ports.FailoverEventFailure, 3600); n != 1 {
		t.Errorf("after delete acc-2 count: got %d want 1", n)
	}
}

func TestFailoverOverridesStore_RoundTrip(t *testing.T) {
	db := openMem(t)
	accts := NewAccountStore(db)
	seedAccount(t, accts, "acc-1")

	store := NewFailoverOverridesStore(db)
	ctx := context.Background()
	got, err := store.Get(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("Get without upsert should return nil, got %+v", got)
	}

	enabled := true
	limit := 5
	judgeCount := 2
	if err := store.Upsert(ctx, &ports.FailoverOverride{
		AccountID:  "acc-1",
		Enabled:    &enabled,
		FailLimit:  &limit,
		JudgeCount: &judgeCount,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("Get returned nil after upsert")
	}
	if got.Enabled == nil || *got.Enabled != true {
		t.Errorf("Enabled: got %v", got.Enabled)
	}
	if got.FailLimit == nil || *got.FailLimit != 5 {
		t.Errorf("FailLimit: got %v", got.FailLimit)
	}
	if got.JudgeCount == nil || *got.JudgeCount != 2 {
		t.Errorf("JudgeCount: got %v", got.JudgeCount)
	}
	if got.CooldownSec != nil {
		t.Errorf("CooldownSec should be nil (not written), got %v", *got.CooldownSec)
	}

	// Delete + re-Get.
	if err := store.Delete(ctx, "acc-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.Get(ctx, "acc-1"); got != nil {
		t.Errorf("Get after Delete: got %+v want nil", got)
	}
}

func TestAccountStore_MarkThrottled_And_RecoverThrottled(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	seedAccount(t, store, "acc-1")

	ctx := context.Background()
	past := time.Now().Add(-1 * time.Minute)
	if err := store.MarkThrottled(ctx, "acc-1", past, "throttle"); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusThrottled {
		t.Fatalf("status after MarkThrottled: got %s want throttled", got.Status)
	}
	if got.StatusReason != "throttle" {
		t.Errorf("status_reason: got %q want throttle", got.StatusReason)
	}
	if got.ThrottledUntil.IsZero() {
		t.Errorf("throttled_until should be set")
	}
	// RecoverThrottled should flip back because the deadline is past.
	n, err := store.RecoverThrottled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("RecoverThrottled: got %d want 1", n)
	}
	got, err = store.Get(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusActive {
		t.Errorf("after recovery status: got %s want active", got.Status)
	}
	if !got.ThrottledUntil.IsZero() {
		t.Errorf("after recovery throttled_until should be cleared, got %v", got.ThrottledUntil)
	}
	if got.StatusReason != "" {
		t.Errorf("after recovery status_reason should be cleared, got %q", got.StatusReason)
	}
}

func TestAccountStore_IncrAndReset_FailStreak(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	seedAccount(t, store, "acc-1")

	ctx := context.Background()
	n, err := store.IncrFailStreak(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("first incr: got %d want 1", n)
	}
	n, _ = store.IncrFailStreak(ctx, "acc-1")
	if n != 2 {
		t.Errorf("second incr: got %d want 2", n)
	}
	if err := store.ResetFailStreak(ctx, "acc-1"); err != nil {
		t.Fatal(err)
	}
	acc, _ := store.Get(ctx, "acc-1")
	if acc.FailStreak != 0 {
		t.Errorf("after reset: got %d want 0", acc.FailStreak)
	}

	// Missing account surfaces the domain sentinel.
	if _, err := store.IncrFailStreak(ctx, "nope"); err == nil {
		t.Errorf("expected error for missing account")
	}
	if err := store.ResetFailStreak(ctx, "nope"); err == nil {
		t.Errorf("expected error for missing account on reset")
	}
}

func TestAccountStore_MarkStatus_PersistsReason(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	seedAccount(t, store, "acc-1")

	ctx := context.Background()
	if err := store.MarkStatus(ctx, "acc-1", domain.StatusDisabled, "consec_fail"); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, "acc-1")
	if got.Status != domain.StatusDisabled {
		t.Errorf("status: got %s want disabled", got.Status)
	}
	if got.StatusReason != "consec_fail" {
		t.Errorf("status_reason: got %q want consec_fail", got.StatusReason)
	}

	// Flipping back to active clears the reason + throttle deadline.
	if err := store.MarkThrottled(ctx, "acc-1", time.Now().Add(1*time.Hour), "throttle"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkStatus(ctx, "acc-1", domain.StatusActive, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(ctx, "acc-1")
	if got.Status != domain.StatusActive {
		t.Errorf("re-active status: got %s want active", got.Status)
	}
	if got.StatusReason != "" {
		t.Errorf("re-active reason: got %q want ''", got.StatusReason)
	}
	if !got.ThrottledUntil.IsZero() {
		t.Errorf("re-active throttled_until: got %v want zero", got.ThrottledUntil)
	}
}

func TestAccountStore_PickAndLock_ConsidersRecoverableThrottled(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	seedAccount(t, store, "acc-1")

	ctx := context.Background()
	// Give acc-1 enough balance to satisfy PickAndLock's MinBalance
	// filter (est_cost * 120% headroom).
	if err := store.UpdateBalance(ctx, "acc-1", 100_000, 0, 0); err != nil {
		t.Fatal(err)
	}
	// Throttle in the past → picker sees it as recoverable.
	if err := store.MarkThrottled(ctx, "acc-1", time.Now().Add(-1*time.Second), "throttle"); err != nil {
		t.Fatal(err)
	}
	acc, tok, err := store.PickAndLock(ctx, ports.PickParams{EstCostHundredths: 100})
	if err != nil {
		t.Fatalf("PickAndLock returned error: %v", err)
	}
	if acc == nil {
		t.Fatal("expected an account, got nil")
	}
	if acc.ID != "acc-1" {
		t.Errorf("picked %s want acc-1", acc.ID)
	}
	_ = tok
	if err := store.Unlock(ctx, acc.ID, tok); err != nil {
		t.Fatal(err)
	}

	// Push the deadline into the future — the picker must skip.
	if err := store.MarkThrottled(ctx, "acc-1", time.Now().Add(1*time.Hour), "throttle"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PickAndLock(ctx, ports.PickParams{EstCostHundredths: 100}); err == nil {
		t.Errorf("expected ErrNoEligibleAccount when only throttled + still cooling; got success")
	}
}
