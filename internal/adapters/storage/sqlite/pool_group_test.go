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

// TestAccountStore_PickAndLock_AccountMaxConcurrent verifies the F4
// audit fix: accounts.max_concurrent column is honoured by PickAndLock.
// Set the row's cap to 1 and prove the second pick fails even when the
// caller passes a looser group-level cap (10) via PickParams.
//
// The account cap must ALSO apply when the group-level cap is looser
// (accounts.max_concurrent=1, PickParams cap=10 → effective cap = 1).
// This is the intended precedence: MIN across both bounds.
func TestAccountStore_PickAndLock_AccountMaxConcurrent(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Single account with column-level cap = 1.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_capped", Email: "capped@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		MaxConcurrent: 1, // <-- the field under test
		RegisteredAt:  time.Now(), ImportedAt: time.Now(),
	}))

	// First pick: allowed (in_flight=0 < column-cap=1).
	if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths:       1000,
		MaxConcurrentPerAccount: 10, // deliberately loose group cap
	}); err != nil {
		t.Fatalf("pick 1: %v", err)
	}
	// Second pick: must fail because in_flight (=1) is not below the
	// column-level cap of 1, even though the group cap (10) still has room.
	if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths:       1000,
		MaxConcurrentPerAccount: 10,
	}); err != domain.ErrNoEligibleAccount {
		t.Errorf("over column-cap pick: got %v want ErrNoEligibleAccount", err)
	}
}

// TestAccountStore_PickAndLock_ZeroAccountCapIsUnlimited verifies the
// column's zero-means-unset semantics: an untouched account row
// (max_concurrent=0, the default) must continue to obey only the group
// / fallback caps, preserving the pre-F4 behaviour for existing
// deployments that never touched this column.
func TestAccountStore_PickAndLock_ZeroAccountCapIsUnlimited(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// max_concurrent left at zero (the schema default).
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_open", Email: "open@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))

	// Three picks under a group cap of 3 must all succeed — the column
	// cap of 0 must NOT be treated as "in_flight < 0".
	for i := 1; i <= 3; i++ {
		if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
			EstCostHundredths:       1000,
			MaxConcurrentPerAccount: 3,
		}); err != nil {
			t.Fatalf("pick %d with zero column cap: %v", i, err)
		}
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

// TestAccountStore_PickAndLock_MinPlan verifies the min-plan gate added
// for seedream-v4-5-style models: a model with MinPlan=basic must skip
// free + starter accounts and land on basic (or higher). This is the
// regression that fixes the "seedream-v4-5 picked a free account → 402"
// bug from the 2026-07-20 operational review.
func TestAccountStore_PickAndLock_MinPlan(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Free account: enough balance, but must be skipped by min_plan=basic.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_free", Email: "free@x.co", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanFree, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	// Basic account: should be chosen.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_basic", Email: "basic@x.co", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanBasic, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))

	// MinPlan=basic must exclude the free account and pick the basic one.
	for i := 1; i <= 3; i++ {
		acc, _, err := accStore.PickAndLock(ctx, ports.PickParams{
			EstCostHundredths: 1000,
			MinPlan:           domain.PlanBasic,
		})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if acc.ID != "acc_basic" {
			t.Fatalf("pick %d: expected acc_basic, got %s (plan=%s)",
				i, acc.ID, acc.PlanType)
		}
	}

	// MinPlan unset (zero value) picks either — both are eligible.
	// Reset in-flight so the basic account is available again for the
	// unrestricted assertion.
	must(accStore.UpdateInFlight(ctx, "acc_basic", -3))
	acc, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
	})
	if err != nil {
		t.Fatalf("pick without MinPlan: %v", err)
	}
	if acc.ID != "acc_free" && acc.ID != "acc_basic" {
		t.Errorf("pick without MinPlan: got %s, want acc_free or acc_basic", acc.ID)
	}
}

// TestAccountStore_PickAndLock_RequiresUnlim confirms the WHERE clause
// on has_unlim = 1 actually gates when the caller sets RequiresUnlim.
// Regression against the 2026-07-20 finding that proxy.Service forgot
// to fill PickParams.RequiresUnlim; even after that wire-up landed, the
// SQL path is the layer of defence that has to catch bad callers.
func TestAccountStore_PickAndLock_RequiresUnlim(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Normal account without unlim.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_normal", Email: "n@x.co", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, HasUnlim: false, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	// Unlim account.
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_unlim", Email: "u@x.co", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000, HasUnlim: true, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))

	acc, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		RequiresUnlim:     true,
	})
	if err != nil {
		t.Fatalf("pick with RequiresUnlim: %v", err)
	}
	if acc.ID != "acc_unlim" {
		t.Errorf("RequiresUnlim: got %s, want acc_unlim", acc.ID)
	}
}

// TestAccountStore_PickAndLock_LoadBalanceIsTierAware is the v0.5.5
// regression: the default 'round_robin' (a.k.a. 'load_balance')
// strategy MUST prefer the closest-tier-above-the-floor account. The
// tier-aware ordering is no longer opt-in via 'best_fit' — every
// load-balanced group gets it. Same setup as _BestFit below; asserts
// starter wins first, pro second, plus third.
func TestAccountStore_PickAndLock_LoadBalanceIsTierAware(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-1 * time.Hour)
	for _, a := range []struct {
		id, email string
		plan      domain.PlanType
	}{
		{"acc_starter_lb", "s@lb.co", domain.PlanStarter},
		{"acc_pro_lb", "p@lb.co", domain.PlanPro},
		{"acc_plus_lb", "u@lb.co", domain.PlanPlus},
	} {
		must(accStore.Upsert(ctx, &domain.Account{
			ID: a.id, Email: a.email, Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
			PlanType: a.plan, SubscriptionBalance: 50000, Status: domain.StatusActive,
			RegisteredAt: base, ImportedAt: base,
		}))
	}

	// RouteStrategy left empty → defaults to RouteRoundRobin (the
	// "load_balance" default). Verify it now behaves like the old
	// best_fit did.
	acc1, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		MinPlan:           domain.PlanStarter,
	})
	if err != nil {
		t.Fatalf("pick 1: %v", err)
	}
	if acc1.ID != "acc_starter_lb" {
		t.Errorf("load_balance pick 1: got %s (plan=%s), want acc_starter_lb",
			acc1.ID, acc1.PlanType)
	}

	acc2, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths:       1000,
		MinPlan:                 domain.PlanStarter,
		MaxConcurrentPerAccount: 1,
	})
	if err != nil {
		t.Fatalf("pick 2: %v", err)
	}
	if acc2.ID != "acc_pro_lb" {
		t.Errorf("load_balance pick 2 (starter busy): got %s (plan=%s), want acc_pro_lb",
			acc2.ID, acc2.PlanType)
	}

	acc3, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths:       1000,
		MinPlan:                 domain.PlanStarter,
		MaxConcurrentPerAccount: 1,
	})
	if err != nil {
		t.Fatalf("pick 3: %v", err)
	}
	if acc3.ID != "acc_plus_lb" {
		t.Errorf("load_balance pick 3 (starter+pro busy): got %s (plan=%s), want acc_plus_lb",
			acc3.ID, acc3.PlanType)
	}
}

// TestAccountStore_PickAndLock_BestFit verifies the legacy RouteBestFit
// alias still works — kept for backward compatibility with rolling
// upgrades and API clients that hold the old string. Since v0.5.5 the
// behaviour is identical to the default round_robin path.
func TestAccountStore_PickAndLock_BestFit(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Three accounts across three tiers, all with enough balance and
	// no in-flight jobs. Same last_used_at so tie-breaking falls to
	// the CASE tier_rank ORDER BY the strategy adds.
	base := time.Now().Add(-1 * time.Hour)
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_starter", Email: "s@x.co", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanStarter, SubscriptionBalance: 50000, Status: domain.StatusActive,
		RegisteredAt: base, ImportedAt: base,
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_pro", Email: "p@x.co", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPro, SubscriptionBalance: 50000, Status: domain.StatusActive,
		RegisteredAt: base, ImportedAt: base,
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_plus", Email: "u@x.co", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 50000, Status: domain.StatusActive,
		RegisteredAt: base, ImportedAt: base,
	}))

	// First pick: starter (rank 1) is closest to the min_plan=starter
	// floor, so it wins over pro (rank 3) and plus (rank 4).
	acc1, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000,
		MinPlan:           domain.PlanStarter,
		RouteStrategy:     domain.RouteBestFit,
	})
	if err != nil {
		t.Fatalf("pick 1: %v", err)
	}
	if acc1.ID != "acc_starter" {
		t.Errorf("best-fit pick 1: got %s (plan=%s), want acc_starter",
			acc1.ID, acc1.PlanType)
	}

	// Fill the starter slot by pushing in_flight up to the cap. Use
	// MaxConcurrentPerAccount=1 to make the first pick's lock exclude
	// it from the second pick.
	// After PickAndLock the starter account already has in_flight=1;
	// with MaxConcurrentPerAccount=1 the WHERE clause excludes it and
	// pro (rank 3) becomes the next best fit.
	acc2, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths:       1000,
		MinPlan:                 domain.PlanStarter,
		RouteStrategy:           domain.RouteBestFit,
		MaxConcurrentPerAccount: 1,
	})
	if err != nil {
		t.Fatalf("pick 2: %v", err)
	}
	if acc2.ID != "acc_pro" {
		t.Errorf("best-fit pick 2 (starter busy): got %s (plan=%s), want acc_pro",
			acc2.ID, acc2.PlanType)
	}

	// Third pick escalates to plus (rank 4).
	acc3, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths:       1000,
		MinPlan:                 domain.PlanStarter,
		RouteStrategy:           domain.RouteBestFit,
		MaxConcurrentPerAccount: 1,
	})
	if err != nil {
		t.Fatalf("pick 3: %v", err)
	}
	if acc3.ID != "acc_plus" {
		t.Errorf("best-fit pick 3 (starter+pro busy): got %s (plan=%s), want acc_plus",
			acc3.ID, acc3.PlanType)
	}
}
