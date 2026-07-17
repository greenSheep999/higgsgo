package sqlite

// Tests for the direct 1:1 API-key -> pool group binding column added by
// migration 005 (api_keys.group_id). These verify:
//
//   * A key written with a non-empty GroupID round-trips cleanly through
//     Create -> Get with the field preserved.
//   * A batch insert of keys with distinct GroupIDs preserves each row's
//     GroupID under List. This guards against a silent scanner regression
//     where a shared column position mismatch could cause values to bleed
//     between rows.
//
// The M:N apikey_group_bindings table (migration 001) is intentionally
// untouched by these tests — its coverage lives in group_store_test.go and
// stays in force to demonstrate that both binding modes coexist.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestAPIKeyStore_CreateWithGroupID(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	k := &domain.APIKey{
		ID:           "key_direct_1",
		KeyHash:      "hash_direct_1",
		Name:         "direct/g1",
		CreatedBy:    "cpa-plugin",
		CPAPartnerID: "p_direct",
		GroupID:      "g1",
		Status:       "active",
		MonthlyQuota: 1000,
		MarkupPct:    1.1,
		CreatedAt:    time.Now().UTC(),
	}
	if err := store.Create(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GroupID != "g1" {
		t.Errorf("GroupID: got %q want g1", got.GroupID)
	}
	// Sanity-check adjacent scanned fields to catch column-order regressions:
	// if group_id landed in the wrong scanner slot we'd see collateral damage
	// on either side of it.
	if got.CPAPartnerID != "p_direct" {
		t.Errorf("CPAPartnerID: got %q want p_direct", got.CPAPartnerID)
	}
	if got.Status != "active" {
		t.Errorf("Status: got %q want active", got.Status)
	}
	if got.MonthlyQuota != 1000 {
		t.Errorf("MonthlyQuota: got %d want 1000", got.MonthlyQuota)
	}
}

func TestAPIKeyStore_ListWithGroupID(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	base := time.Now().UTC()
	rows := []*domain.APIKey{
		{ID: "k_g1", KeyHash: "h1", Name: "g1", GroupID: "g1", CreatedBy: "cpa-plugin", CreatedAt: base},
		{ID: "k_g2", KeyHash: "h2", Name: "g2", GroupID: "g2", CreatedBy: "cpa-plugin", CreatedAt: base.Add(time.Second)},
		// A key without direct binding must survive with GroupID = "" so
		// the resolveGroup fall-back path (M:N) stays reachable.
		{ID: "k_plain", KeyHash: "h3", Name: "plain", CreatedBy: "admin", CreatedAt: base.Add(2 * time.Second)},
	}
	for _, k := range rows {
		if err := store.Create(ctx, k); err != nil {
			t.Fatalf("create %s: %v", k.ID, err)
		}
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("list len: got %d want 3", len(list))
	}
	// Verify each id → GroupID pairing survived the round trip.
	byID := make(map[string]string, len(list))
	for _, k := range list {
		byID[k.ID] = k.GroupID
	}
	want := map[string]string{"k_g1": "g1", "k_g2": "g2", "k_plain": ""}
	for id, wg := range want {
		if got, ok := byID[id]; !ok {
			t.Errorf("missing row %s from list", id)
		} else if got != wg {
			t.Errorf("GroupID for %s: got %q want %q", id, got, wg)
		}
	}
}
