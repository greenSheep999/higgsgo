package sqlite

// Tests for the LoadBalanceOpts hook on PickAndLock. Two scenarios:
//
//   1. tier_aware=false — the cheap-first plan-tier CASE is skipped so
//      a `plus` account can win over a `starter` account when the plus
//      row has the older LRU stamp. Under the historical hardcoded
//      ordering the starter row would always come first regardless of
//      LRU, because plan-tier rank was the primary sort key.
//   2. balance_headroom_pct=100 — the pool accepts an account whose
//      subscription_balance == estimated cost (no headroom buffer).
//      Under the historical hardcoded formula (cost + cost/5) the same
//      account would be filtered out because balance < cost * 1.2.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestAccountStore_PickAndLock_LoadBalanceTierAwareOff confirms that
// disabling tier_aware demotes the plan-tier CASE from the ORDER BY,
// so LRU wins on its own. Seeds two accounts: a starter with a fresh
// last_used_at (recent) and a plus with an old last_used_at (stale).
// With tier_aware=true (default) the starter wins because starter=1 <
// plus=4. With tier_aware=false the plus wins because its LRU stamp is
// older.
func TestAccountStore_PickAndLock_LoadBalanceTierAwareOff(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	starterLRU := now.Add(-1 * time.Minute)   // recent
	plusLRU := now.Add(-24 * time.Hour)       // stale
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_starter", Email: "starter@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanStarter, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: starterLRU,
		RegisteredAt: now, ImportedAt: now,
	}))
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_plus", Email: "plus@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: plusLRU,
		RegisteredAt: now, ImportedAt: now,
	}))

	// Baseline: default hardcoded ordering (tier-aware ON) → starter wins.
	got, tok, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		RouteStrategy:     domain.RouteRoundRobin,
	})
	if err != nil {
		t.Fatalf("baseline pick: %v", err)
	}
	if got.ID != "acc_starter" {
		t.Errorf("baseline: got %s want acc_starter (tier ranking should win)", got.ID)
	}
	must(store.Unlock(ctx, got.ID, tok))

	// Reset LRU on both rows so the deterministic tail below re-runs
	// against the seeded values (Unlock+Pick above just moved
	// acc_starter's last_used_at forward, which would also flip the
	// second assertion for the wrong reason).
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_starter", Email: "starter@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanStarter, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: starterLRU,
		RegisteredAt: now, ImportedAt: now,
	}))

	// tier_aware=false + jitter=false so the pick is deterministic:
	// plus wins on LRU because starter's stamp is more recent.
	got2, tok2, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		RouteStrategy:     domain.RouteRoundRobin,
		LoadBalance: ports.LoadBalanceOpts{
			Populated:          true,
			TierAware:          false,
			BalanceHeadroomPct: 120,
			Jitter:             false,
		},
	})
	if err != nil {
		t.Fatalf("tier-aware-off pick: %v", err)
	}
	if got2.ID != "acc_plus" {
		t.Errorf("tier_aware=false: got %s want acc_plus (LRU should win when tier ranking is disabled)", got2.ID)
	}
	must(store.Unlock(ctx, got2.ID, tok2))
}

// TestAccountStore_PickAndLock_HeadroomTight confirms that setting
// balance_headroom_pct=100 relaxes the balance filter enough for an
// account whose subscription_balance is exactly the estimated cost to
// qualify. Under the historical hardcoded formula (cost + cost/5, i.e.
// 120%) the same account would be filtered out by the WHERE clause.
func TestAccountStore_PickAndLock_HeadroomTight(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// One account with balance == cost exactly. No LRU set so the
	// filter is the only knob under test.
	now := time.Now().UTC()
	const cost = int64(1000)
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_tight", Email: "tight@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: cost,
		Status: domain.StatusActive,
		RegisteredAt: now, ImportedAt: now,
	}))

	// Baseline: default 120% headroom → account filtered out.
	_, _, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: cost,
		RouteStrategy:     domain.RouteRoundRobin,
	})
	if err == nil {
		t.Fatalf("baseline: expected ErrNoEligibleAccount at 120%% headroom, got success")
	}

	// headroom=100 → account qualifies (balance >= cost * 1.0).
	got, tok, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: cost,
		RouteStrategy:     domain.RouteRoundRobin,
		LoadBalance: ports.LoadBalanceOpts{
			Populated:          true,
			TierAware:          true,
			BalanceHeadroomPct: 100,
			Jitter:             false,
		},
	})
	if err != nil {
		t.Fatalf("headroom=100 pick: %v", err)
	}
	if got.ID != "acc_tight" {
		t.Errorf("headroom=100: got %s want acc_tight", got.ID)
	}
	must(store.Unlock(ctx, got.ID, tok))
}
