package sqlite

// Regression test for ROADMAP P2-8: with three otherwise-identical
// accounts, a burst of picks must land on all three rather than always
// hitting the row SQLite happens to store first.
//
// Before the tail `, in_flight_jobs ASC, RANDOM() LIMIT 1` was added,
// this test would fail deterministically — the natural row-order
// tiebreaker sent every pick to the same account until its
// last_used_at commit advanced.
//
// The check is deliberately loose ("hit at least 2 of 3 rows") to
// avoid flakes on tiny sample sizes; the point is to prove picks
// spread, not to characterise a specific distribution.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func TestPickAndLock_JitterSpreadsAcrossTiedAccounts(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Seed three accounts with identical primary sort keys. Same
	// plan type, same balance, same LRU stamp (fresh insert = NULL,
	// which the COALESCE default equalises). Without the jitter tail
	// SQLite consistently returns row 0.
	seed := time.Now()
	for i, id := range []string{"acc_a", "acc_b", "acc_c"} {
		must(accStore.Upsert(ctx, &domain.Account{
			ID: id, Email: id + "@example.com",
			Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
			PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
			Status: domain.StatusActive,
			// Same second so the LRU key is a true tie.
			RegisteredAt: seed.Add(time.Duration(i) * time.Nanosecond),
			ImportedAt:   seed,
		}))
	}

	// 30 picks each with immediate Unlock so in_flight_jobs is a
	// weak differentiator (RouteRoundRobin's tail evaluates it
	// second, and it will move around as picks are in-flight). This
	// exercises the RANDOM() fallback more than the in_flight_jobs
	// tiebreaker.
	hits := map[string]int{}
	for i := 0; i < 30; i++ {
		acc, tok, err := accStore.PickAndLock(ctx, ports.PickParams{
			EstCostHundredths: 100,
			RouteStrategy:     domain.RouteRoundRobin,
		})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		hits[acc.ID]++
		must(accStore.Unlock(ctx, acc.ID, tok))
	}

	if len(hits) < 2 {
		t.Fatalf("expected picks to spread across at least 2 of 3 rows; got %v", hits)
	}
	// Guard against a single row absorbing the entire burst — that's
	// the failure mode P2-8 fixes.
	for id, n := range hits {
		if n == 30 {
			t.Errorf("row %s absorbed all 30 picks; jitter tail did not fire", id)
		}
	}
}
