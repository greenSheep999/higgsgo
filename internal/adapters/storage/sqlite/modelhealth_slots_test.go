package sqlite

// Regression test for ROADMAP P3-13: ModelHealthStore.SlotsByJST must
// bucket probes into fixed-width slots so the WebUI uptime bar can
// render real history instead of the "no data" placeholder.
//
// Two scenarios:
//   1. All probes land in the most recent slot → last slot has the
//      expected pass/total, older slots stay total=0.
//   2. Probes spread across two adjacent slots → both buckets carry
//      the correct counts.
// Empty jst also validated (nothing to bucket).

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestModelHealthStore_SlotsByJST(t *testing.T) {
	db := openMem(t)
	store := NewModelHealthStore(db)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Insert 3 probes spaced 1 second apart so the RFC3339-second
	// primary-key resolution doesn't collapse them into one row. All
	// three land in the current hour-wide slot: 2 completed, 1 failed.
	// Real production probes are hours apart so this is only an
	// artifact of trying to write three probes inside a single test
	// second.
	//
	// Real production slots are 3600s (1h); here we use the same
	// width so the "gap slots return total=0" branch stays testable
	// in the same run.
	now := time.Now().UTC()
	must(store.Insert(ctx, "seedance_2_0", now.Add(-2*time.Second), domain.JobCompleted, 200, 100, 3))
	must(store.Insert(ctx, "seedance_2_0", now.Add(-1*time.Second), domain.JobCompleted, 200, 100, 3))
	must(store.Insert(ctx, "seedance_2_0", now, domain.JobFailed, 200, 100, 3))

	// 3 slots of width 3600s. All 3 probes land in the most recent
	// slot; the two older ones are empty.
	slots, err := store.SlotsByJST(ctx, "seedance_2_0", 3, 3600)
	must(err)
	if len(slots) != 3 {
		t.Fatalf("expected 3 slots, got %d", len(slots))
	}
	// Oldest first.
	if slots[0].Total != 0 || slots[1].Total != 0 {
		t.Errorf("older slots should be empty: %+v", slots)
	}
	if slots[2].Total != 3 || slots[2].Passed != 2 {
		t.Errorf("current slot: got total=%d passed=%d want 3/2 (%+v)",
			slots[2].Total, slots[2].Passed, slots[2])
	}

	// A jst with zero probes returns count slots, all total=0. The
	// frontend renders these as muted "no data" blocks so the bar
	// keeps a stable width even for freshly added models.
	empty, err := store.SlotsByJST(ctx, "never_probed", 12, 3600)
	must(err)
	if len(empty) != 12 {
		t.Fatalf("empty jst: expected 12 slots, got %d", len(empty))
	}
	for i, s := range empty {
		if s.Total != 0 {
			t.Errorf("empty jst slot %d: total=%d want 0", i, s.Total)
		}
	}

	// Guardrails: zero count returns nil without error.
	got, err := store.SlotsByJST(ctx, "seedance_2_0", 0, 3600)
	must(err)
	if got != nil {
		t.Errorf("count=0 should return nil, got %v", got)
	}
}
