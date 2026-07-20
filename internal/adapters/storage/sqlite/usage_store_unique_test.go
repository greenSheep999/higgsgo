package sqlite

// Tests for the UNIQUE index on usage_events(higgsgo_job_id) added by
// migration 018 (F1 defence in depth). The application-level fix in
// core/proxy + core/pollworker gates metering on a compare-and-swap so only
// one observer inserts, but if that gate ever regresses the index must
// still stop the double insert at the storage layer.

import (
	"context"
	"errors"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestUsageStore_UniqueOnHigghjobID(t *testing.T) {
	db := openMem(t)
	store := NewUsageEventStore(db)
	ctx := context.Background()

	// First insert succeeds — the row is the sync path's usage_event.
	first := newUsageEvent("winner")
	first.HiggsgoJobID = "job_dup_1"
	if err := store.Insert(ctx, first); err != nil {
		t.Fatalf("insert winner: %v", err)
	}

	// Second insert collides on higgsgo_job_id. The store must map the
	// UNIQUE constraint failure to domain.ErrUsageEventDuplicate so a
	// caller like the pollworker can treat it as "already recorded,
	// skip" rather than a real store failure.
	loser := newUsageEvent("loser")
	loser.HiggsgoJobID = "job_dup_1"
	err := store.Insert(ctx, loser)
	if err == nil {
		t.Fatalf("second insert with same higgsgo_job_id: expected error, got nil")
	}
	if !errors.Is(err, domain.ErrUsageEventDuplicate) {
		t.Fatalf("second insert error: got %v, want ErrUsageEventDuplicate", err)
	}

	// Sanity check: the winner's row is the one that survived. The
	// loser's id must not appear.
	rows, err := db.Query(`SELECT id FROM usage_events WHERE higgsgo_job_id = ?`, "job_dup_1")
	if err != nil {
		t.Fatalf("verify query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != "winner" {
		t.Errorf("survivors for job_dup_1: got %v, want [winner]", ids)
	}
}

// TestUsageStore_CFPrefixIDsCoexistUnderUnique documents the intentional
// choice to use a FULL UNIQUE index (not a partial one filtering out cf_*
// synthetic ids). idgen.NewID("cf") mints ids with a random suffix on every
// call, so distinct sync-path CreateJob failures produce distinct rows and
// the full index does not falsely reject them.
func TestUsageStore_CFPrefixIDsCoexistUnderUnique(t *testing.T) {
	db := openMem(t)
	store := NewUsageEventStore(db)
	ctx := context.Background()

	// Two distinct cf_* higgsgo_job_ids — mirrors two independent
	// CreateJob failures against different accounts on the sync path.
	e1 := newUsageEvent("u_cf_1")
	e1.HiggsgoJobID = "cf_deadbeef00000001aa"
	e2 := newUsageEvent("u_cf_2")
	e2.HiggsgoJobID = "cf_deadbeef00000001bb"

	if err := store.Insert(ctx, e1); err != nil {
		t.Fatalf("insert cf_1: %v", err)
	}
	if err := store.Insert(ctx, e2); err != nil {
		t.Fatalf("insert cf_2 (distinct id): %v — full unique index rejected a legitimate row", err)
	}
}
