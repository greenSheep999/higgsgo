package sqlite

// Group-scoped pool pick regression test: verifies that
// AccountStore.PickAndLock honours PickParams.GroupID by restricting the
// candidate set to accounts joined via account_group_members. Without this
// guard, a group with an ultra plan could bleed traffic into another
// group's accounts.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func TestAccountStore_PickAndLock_GroupScoped(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	grpStore := NewGroupStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Two groups, each with one plus-plan account.
	must(grpStore.Create(ctx, sampleGroup("grp_1", "g1")))
	must(grpStore.Create(ctx, sampleGroup("grp_2", "g2")))

	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_g1", Email: "g1@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_g2", Email: "g2@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	// Third account that belongs to no group — must never be picked when
	// GroupID is set on the pick params.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_orphan", Email: "orphan@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))

	must(grpStore.AddMember(ctx, "grp_1", "acc_g1", 100))
	must(grpStore.AddMember(ctx, "grp_2", "acc_g2", 100))

	// Picking against grp_1 must return acc_g1 (never acc_g2 or acc_orphan).
	got, tok, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		GroupID:           "grp_1",
	})
	if err != nil {
		t.Fatalf("pick grp_1: %v", err)
	}
	if got.ID != "acc_g1" {
		t.Errorf("grp_1 pick: got %s want acc_g1", got.ID)
	}
	must(accStore.Unlock(ctx, got.ID, tok))

	// Picking against grp_2 must return acc_g2.
	got, tok, err = accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		GroupID:           "grp_2",
	})
	if err != nil {
		t.Fatalf("pick grp_2: %v", err)
	}
	if got.ID != "acc_g2" {
		t.Errorf("grp_2 pick: got %s want acc_g2", got.ID)
	}
	must(accStore.Unlock(ctx, got.ID, tok))

	// Picking against a group with no members must return ErrNoEligibleAccount
	// even though the orphan account would otherwise be a valid candidate.
	must(grpStore.Create(ctx, sampleGroup("grp_empty", "empty")))
	if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		GroupID:           "grp_empty",
	}); err != domain.ErrNoEligibleAccount {
		t.Errorf("empty group pick: got %v want ErrNoEligibleAccount", err)
	}
}
