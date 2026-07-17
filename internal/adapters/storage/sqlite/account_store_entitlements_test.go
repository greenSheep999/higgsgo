package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestAccountStore_UpdateEntitlements exercises the refresher-writer path:
// entitlement-only fields must be overwritten, and pre-existing balance
// fields (subscription_balance, credits_balance) must be left alone.
func TestAccountStore_UpdateEntitlements(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	// Baseline account: plus tier, no unlim flags, some balance.
	base := &domain.Account{
		ID:                  "user_ent_1",
		Email:               "ent1@example.com",
		Password:            "-",
		SessionID:           "sess",
		CookiesJSON:         "{}",
		UserAgent:           "-",
		PlanType:            domain.PlanPlus,
		HasUnlim:            false,
		HasFlexUnlim:        false,
		IsProVeo3Available:  false,
		Cohort:              "cohort_old",
		SubscriptionBalance: 55555, // must survive the entitlement update
		CreditsBalance:      12345, // must survive too
		TotalPlanCredits:    10000,
		Status:              domain.StatusActive,
		RegisteredAt:        time.Now().UTC(),
		ImportedAt:          time.Now().UTC(),
	}
	if err := store.Upsert(ctx, base); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	planEndsAt := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	upd := ports.EntitlementUpdate{
		PlanType:           domain.PlanUltra,
		HasUnlim:           true,
		HasFlexUnlim:       true,
		IsProVeo3Available: true,
		Cohort:             "cohort_new",
		TotalPlanCredits:   99900,
		PlanEndsAt:         planEndsAt,
	}
	if err := store.UpdateEntitlements(ctx, base.ID, upd); err != nil {
		t.Fatalf("UpdateEntitlements: %v", err)
	}

	got, err := store.Get(ctx, base.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Entitlement fields must reflect the update.
	if got.PlanType != domain.PlanUltra {
		t.Errorf("PlanType: got %s want ultra", got.PlanType)
	}
	if !got.HasUnlim {
		t.Errorf("HasUnlim: got false want true")
	}
	if !got.HasFlexUnlim {
		t.Errorf("HasFlexUnlim: got false want true")
	}
	if !got.IsProVeo3Available {
		t.Errorf("IsProVeo3Available: got false want true")
	}
	if got.Cohort != "cohort_new" {
		t.Errorf("Cohort: got %q want cohort_new", got.Cohort)
	}
	if got.TotalPlanCredits != 99900 {
		t.Errorf("TotalPlanCredits: got %d want 99900", got.TotalPlanCredits)
	}
	if !got.PlanEndsAt.Equal(planEndsAt) {
		t.Errorf("PlanEndsAt: got %s want %s", got.PlanEndsAt, planEndsAt)
	}

	// Balance-tier fields must be untouched.
	if got.SubscriptionBalance != 55555 {
		t.Errorf("SubscriptionBalance mutated: got %d want 55555", got.SubscriptionBalance)
	}
	if got.CreditsBalance != 12345 {
		t.Errorf("CreditsBalance mutated: got %d want 12345", got.CreditsBalance)
	}
}

// TestAccountStore_UpdateEntitlements_UnknownID mirrors the behavior of
// UpdateInFlight / MarkStatus: a missing row surfaces as ErrAccountNotFound
// rather than silently succeeding.
func TestAccountStore_UpdateEntitlements_UnknownID(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	err := store.UpdateEntitlements(ctx, "user_does_not_exist", ports.EntitlementUpdate{
		PlanType: domain.PlanUltra,
	})
	if !errors.Is(err, domain.ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}
}
