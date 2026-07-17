package sqlite

// Tests for GroupStore: CRUD, membership, api-key bindings, quota
// accounting, and CurrentInFlight aggregation. All backed by an in-memory
// SQLite handle via openMem (see sqlite_test.go).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// sampleGroup returns a domain.Group with sensible defaults so individual
// tests only need to override the fields they care about.
func sampleGroup(id, name string) *domain.Group {
	return &domain.Group{
		ID:                      id,
		Name:                    name,
		Description:             "test group " + name,
		MaxConcurrentJobs:       10,
		MaxConcurrentPerAccount: 5,
		MonthlyCreditBudget:     100000,
		AllowedModelsRegex:      ".*",
		RouteStrategy:           domain.RouteRoundRobin,
		OwnerType:               domain.OwnerInternal,
		OwnerID:                 "operator",
		Status:                  "active",
		CreatedAt:               time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
}

func TestGroupStore_CreateGetList(t *testing.T) {
	db := openMem(t)
	store := NewGroupStore(db)
	ctx := context.Background()

	g1 := sampleGroup("grp_a", "alpha")
	g2 := sampleGroup("grp_b", "bravo")

	if err := store.Create(ctx, g1); err != nil {
		t.Fatalf("create g1: %v", err)
	}
	if err := store.Create(ctx, g2); err != nil {
		t.Fatalf("create g2: %v", err)
	}

	got, err := store.Get(ctx, g1.ID)
	if err != nil {
		t.Fatalf("get g1: %v", err)
	}
	if got.Name != "alpha" {
		t.Errorf("name: got %q want %q", got.Name, "alpha")
	}
	if got.MonthlyCreditBudget != 100000 {
		t.Errorf("budget: got %d want 100000", got.MonthlyCreditBudget)
	}
	if got.RouteStrategy != domain.RouteRoundRobin {
		t.Errorf("route: got %q want round_robin", got.RouteStrategy)
	}

	// GetByName round-trip.
	byName, err := store.GetByName(ctx, "bravo")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if byName.ID != g2.ID {
		t.Errorf("by name: got %q want %q", byName.ID, g2.ID)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len: got %d want 2", len(list))
	}
	// Alphabetical order (alpha, bravo).
	if list[0].Name != "alpha" || list[1].Name != "bravo" {
		t.Errorf("list order: got %s,%s want alpha,bravo", list[0].Name, list[1].Name)
	}
}

func TestGroupStore_UpdateAndDelete(t *testing.T) {
	db := openMem(t)
	store := NewGroupStore(db)
	ctx := context.Background()

	g := sampleGroup("grp_u", "update-me")
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}

	g.Description = "updated description"
	g.MonthlyCreditBudget = 500000
	g.RouteStrategy = domain.RouteMostCreditsFirst
	if err := store.Update(ctx, g); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := store.Get(ctx, g.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Description != "updated description" {
		t.Errorf("description: got %q", got.Description)
	}
	if got.MonthlyCreditBudget != 500000 {
		t.Errorf("budget: got %d", got.MonthlyCreditBudget)
	}
	if got.RouteStrategy != domain.RouteMostCreditsFirst {
		t.Errorf("route: got %q", got.RouteStrategy)
	}

	// Update on a non-existent id must return ErrGroupNotFound.
	missing := sampleGroup("grp_missing", "nope")
	if err := store.Update(ctx, missing); !errors.Is(err, domain.ErrGroupNotFound) {
		t.Errorf("update missing: got %v want ErrGroupNotFound", err)
	}

	if err := store.Delete(ctx, g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, g.ID); !errors.Is(err, domain.ErrGroupNotFound) {
		t.Errorf("get after delete: got %v want ErrGroupNotFound", err)
	}
	// Second delete is idempotent from the caller's view but reports missing.
	if err := store.Delete(ctx, g.ID); !errors.Is(err, domain.ErrGroupNotFound) {
		t.Errorf("delete missing: got %v want ErrGroupNotFound", err)
	}
}

func TestGroupStore_MembershipRoundTrip(t *testing.T) {
	db := openMem(t)
	store := NewGroupStore(db)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	g := sampleGroup("grp_m", "members")
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("create group: %v", err)
	}

	// Insert two accounts so the FK on account_group_members is satisfied.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_1", Email: "acc1@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_2", Email: "acc2@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))

	if err := store.AddMember(ctx, g.ID, "acc_1", 100); err != nil {
		t.Fatalf("add member 1: %v", err)
	}
	if err := store.AddMember(ctx, g.ID, "acc_2", 200); err != nil {
		t.Fatalf("add member 2: %v", err)
	}
	// Idempotent: AddMember with an existing pair must not error.
	if err := store.AddMember(ctx, g.ID, "acc_1", 100); err != nil {
		t.Fatalf("add member 1 again: %v", err)
	}

	members, err := store.ListMembers(ctx, g.ID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("member count: got %d want 2", len(members))
	}
	// Ordered by priority DESC, so acc_2 (200) comes first.
	if members[0] != "acc_2" || members[1] != "acc_1" {
		t.Errorf("order: got %v want [acc_2 acc_1]", members)
	}

	if err := store.RemoveMember(ctx, g.ID, "acc_1"); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	after, err := store.ListMembers(ctx, g.ID)
	if err != nil {
		t.Fatalf("list after remove: %v", err)
	}
	if len(after) != 1 || after[0] != "acc_2" {
		t.Errorf("after remove: got %v want [acc_2]", after)
	}
	// Remove of a non-existent pair is a no-op (returns nil).
	if err := store.RemoveMember(ctx, g.ID, "acc_missing"); err != nil {
		t.Errorf("remove missing: %v", err)
	}
}

func TestGroupStore_BindAPIKeyRoundTrip(t *testing.T) {
	db := openMem(t)
	store := NewGroupStore(db)
	keyStore := NewAPIKeyStore(db)
	ctx := context.Background()

	g1 := sampleGroup("grp_b1", "bind1")
	g2 := sampleGroup("grp_b2", "bind2")
	if err := store.Create(ctx, g1); err != nil {
		t.Fatalf("create g1: %v", err)
	}
	if err := store.Create(ctx, g2); err != nil {
		t.Fatalf("create g2: %v", err)
	}

	// Insert an api key so the FK on apikey_group_bindings is satisfied.
	if err := keyStore.Create(ctx, &domain.APIKey{
		ID: "key_1", KeyHash: "hash_1", Name: "test", Status: "active",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create key: %v", err)
	}

	if err := store.BindAPIKey(ctx, "key_1", g1.ID); err != nil {
		t.Fatalf("bind g1: %v", err)
	}
	if err := store.BindAPIKey(ctx, "key_1", g2.ID); err != nil {
		t.Fatalf("bind g2: %v", err)
	}
	// Idempotent bind.
	if err := store.BindAPIKey(ctx, "key_1", g1.ID); err != nil {
		t.Fatalf("bind g1 again: %v", err)
	}

	groups, err := store.ListGroupsForAPIKey(ctx, "key_1")
	if err != nil {
		t.Fatalf("list groups for key: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("group count: got %d want 2", len(groups))
	}
	// Ordered by group name (bind1, bind2).
	if groups[0].Name != "bind1" || groups[1].Name != "bind2" {
		t.Errorf("group order: got %s,%s", groups[0].Name, groups[1].Name)
	}

	if err := store.UnbindAPIKey(ctx, "key_1", g1.ID); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	after, err := store.ListGroupsForAPIKey(ctx, "key_1")
	if err != nil {
		t.Fatalf("list after unbind: %v", err)
	}
	if len(after) != 1 || after[0].Name != "bind2" {
		t.Errorf("after unbind: got %v", after)
	}
}

func TestGroupStore_IncrementUsedAndInFlight(t *testing.T) {
	db := openMem(t)
	store := NewGroupStore(db)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	g := sampleGroup("grp_q", "quota")
	if err := store.Create(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := store.IncrementUsed(ctx, g.ID, 1500); err != nil {
		t.Fatalf("increment 1: %v", err)
	}
	if err := store.IncrementUsed(ctx, g.ID, 500); err != nil {
		t.Fatalf("increment 2: %v", err)
	}

	got, err := store.Get(ctx, g.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.MonthlyCreditUsed != 2000 {
		t.Errorf("monthly_used: got %d want 2000", got.MonthlyCreditUsed)
	}

	// IncrementUsed on a missing group must report ErrGroupNotFound.
	if err := store.IncrementUsed(ctx, "grp_missing", 100); !errors.Is(err, domain.ErrGroupNotFound) {
		t.Errorf("increment missing: got %v want ErrGroupNotFound", err)
	}

	// CurrentInFlight: seed two accounts with distinct in_flight counters,
	// only one is a member of g.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_in_1", Email: "in1@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_out_1", Email: "out1@example.com", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.UpdateInFlight(ctx, "acc_in_1", 2))
	must(accStore.UpdateInFlight(ctx, "acc_out_1", 5))
	must(store.AddMember(ctx, g.ID, "acc_in_1", 100))

	n, err := store.CurrentInFlight(ctx, g.ID)
	if err != nil {
		t.Fatalf("current in-flight: %v", err)
	}
	if n != 2 {
		t.Errorf("in-flight: got %d want 2 (only acc_in_1 is a member)", n)
	}
}

func TestGroupStore_GetNotFound(t *testing.T) {
	db := openMem(t)
	store := NewGroupStore(db)
	ctx := context.Background()

	if _, err := store.Get(ctx, "grp_missing"); !errors.Is(err, domain.ErrGroupNotFound) {
		t.Errorf("get missing: got %v want ErrGroupNotFound", err)
	}
	if _, err := store.GetByName(ctx, "missing"); !errors.Is(err, domain.ErrGroupNotFound) {
		t.Errorf("get by name missing: got %v want ErrGroupNotFound", err)
	}
}
