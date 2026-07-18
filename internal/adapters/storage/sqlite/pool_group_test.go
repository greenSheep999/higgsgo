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

// TestAccountStore_PickAndLock_GroupConcurrencyCap verifies that
// PickParams.MaxGroupInFlight enforces a SUM(in_flight_jobs) across a
// group's members. This is the ROADMAP P0-3 enforcement that previously
// existed only as a domain field with no runtime effect.
func TestAccountStore_PickAndLock_GroupConcurrencyCap(t *testing.T) {
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

	// A group with two members. Cap at 3 in-flight jobs across the
	// group. Give each account plenty of per-row headroom so only the
	// group cap can bite.
	must(grpStore.Create(ctx, sampleGroup("grp_cap", "cap")))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_a", Email: "a@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_b", Email: "b@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(grpStore.AddMember(ctx, "grp_cap", "acc_a", 100))
	must(grpStore.AddMember(ctx, "grp_cap", "acc_b", 100))

	// Three picks succeed (across both accounts) — sum-in-flight
	// climbs 0 → 1 → 2 → 3.
	pickOK := func(step int) {
		t.Helper()
		if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
			EstCostHundredths: 1000,
			GroupID:           "grp_cap",
			MaxGroupInFlight:  3,
		}); err != nil {
			t.Fatalf("pick step %d: %v", step, err)
		}
	}
	pickOK(1)
	pickOK(2)
	pickOK(3)

	// Fourth pick must trip the group cap.
	if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		GroupID:           "grp_cap",
		MaxGroupInFlight:  3,
	}); err != domain.ErrGroupConcurrencyMax {
		t.Errorf("saturated pick: got %v want ErrGroupConcurrencyMax", err)
	}

	// Sanity: MaxGroupInFlight = 0 (unset) still permits a pick.
	if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		GroupID:           "grp_cap",
	}); err != nil {
		t.Errorf("uncapped pick: got %v want success", err)
	}
}

// TestAccountStore_PickAndLock_PerAccountCap verifies that
// PickParams.MaxConcurrentPerAccount overrides the historical hardcoded
// literal 5. Set the cap to 2 and prove the third pick on a single
// account fails.
func TestAccountStore_PickAndLock_PerAccountCap(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Single account, no group. Cap at 2 concurrent picks per account.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_solo", Email: "s@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))

	for i := 1; i <= 2; i++ {
		if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
			EstCostHundredths:       1000,
			MaxConcurrentPerAccount: 2,
		}); err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
	}
	// Third pick must fail with ErrNoEligibleAccount — the row's
	// in_flight_jobs (=2) is not below the cap.
	if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths:       1000,
		MaxConcurrentPerAccount: 2,
	}); err != domain.ErrNoEligibleAccount {
		t.Errorf("over-cap pick: got %v want ErrNoEligibleAccount", err)
	}
}

// TestAccountStore_ResetAllInFlight verifies the boot-time reconciliation
// helper that clears leaked in_flight_jobs counters from prior crashes
// (see docs/ROADMAP.md P0-2).
func TestAccountStore_ResetAllInFlight(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Seed two accounts, bump one to in_flight=3 by picking + not
	// unlocking, leave the other at 0.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_leaked", Email: "leak@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_zero", Email: "zero@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.UpdateInFlight(ctx, "acc_leaked", 3))

	// Confirm setup.
	got, err := accStore.Get(ctx, "acc_leaked")
	must(err)
	if got.InFlightJobs != 3 {
		t.Fatalf("setup: got in_flight=%d want 3", got.InFlightJobs)
	}

	// Reset: should report exactly one row changed.
	n, err := accStore.ResetAllInFlight(ctx)
	must(err)
	if n != 1 {
		t.Errorf("reset: got %d rows want 1", n)
	}

	got, err = accStore.Get(ctx, "acc_leaked")
	must(err)
	if got.InFlightJobs != 0 {
		t.Errorf("post-reset acc_leaked: got in_flight=%d want 0", got.InFlightJobs)
	}
	// Idempotent: second reset is a no-op.
	n, err = accStore.ResetAllInFlight(ctx)
	must(err)
	if n != 0 {
		t.Errorf("re-reset: got %d rows want 0", n)
	}
}
