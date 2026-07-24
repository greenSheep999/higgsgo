package sqlite

// Test for UsageEventStore.SumChargedCreditsHForAccount — the aggregation
// used by the monthly credit-ledger reconciler (core/creditrecon). Checks
// the half-open window semantics, per-account isolation, and the empty
// result path returning 0 (not an error).

import (
	"context"
	"testing"
	"time"
)

func TestUsageEventStore_SumChargedCreditsHForAccount(t *testing.T) {
	db := openMem(t)
	store := NewUsageEventStore(db)
	ctx := context.Background()

	// Seed events across two accounts and two months so the query has
	// something to filter on.
	junStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	julStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	augStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	e := func(id, acc string, ts time.Time, charged int64) {
		ev := newUsageEvent(id)
		ev.TS = ts
		ev.AccountID = acc
		ev.ChargedCreditsHundredths = charged
		ev.BillingMonth = ts.Format("2006-01")
		ev.BillingDay = ts.Format("2006-01-02")
		if err := store.Insert(ctx, ev); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	e("s1", "acc_1", junStart.Add(24*time.Hour), 500)         // June, acc_1
	e("s2", "acc_1", junStart.Add(15*24*time.Hour), 1500)     // June, acc_1
	e("s3", "acc_2", junStart.Add(24*time.Hour), 999)         // June, acc_2 — must not leak
	e("s4", "acc_1", julStart.Add(3*24*time.Hour), 700)       // July, acc_1
	e("s5", "acc_1", junStart.Add(-1*time.Hour), 100)         // May 31, acc_1 — must not leak

	// Full-June window for acc_1 = 500 + 1500 = 2000.
	got, err := store.SumChargedCreditsHForAccount(ctx, "acc_1", junStart, julStart)
	if err != nil {
		t.Fatalf("SumChargedCreditsHForAccount: %v", err)
	}
	if got != 2000 {
		t.Errorf("acc_1 June sum: got %d want 2000", got)
	}

	// July window for acc_1 = 700.
	got, err = store.SumChargedCreditsHForAccount(ctx, "acc_1", julStart, augStart)
	if err != nil {
		t.Fatalf("SumChargedCreditsHForAccount July: %v", err)
	}
	if got != 700 {
		t.Errorf("acc_1 July sum: got %d want 700", got)
	}

	// Cross-account isolation: acc_2 June = 999.
	got, err = store.SumChargedCreditsHForAccount(ctx, "acc_2", junStart, julStart)
	if err != nil {
		t.Fatalf("SumChargedCreditsHForAccount acc_2: %v", err)
	}
	if got != 999 {
		t.Errorf("acc_2 June sum: got %d want 999", got)
	}

	// Empty result → 0, no error. Use a month with no rows.
	got, err = store.SumChargedCreditsHForAccount(ctx, "acc_1", augStart, augStart.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("SumChargedCreditsHForAccount August: %v", err)
	}
	if got != 0 {
		t.Errorf("acc_1 August sum: got %d want 0", got)
	}
}
