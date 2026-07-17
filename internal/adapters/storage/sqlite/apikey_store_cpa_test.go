package sqlite

// Tests for the CPA partner column added by migration 004. These verify:
//
//   * A key written with a non-empty CPAPartnerID survives a round trip
//     through Create -> Get with the field preserved.
//   * ListByCPAPartner returns only the rows scoped to the given partner
//     and orders them newest-first (matching List semantics).
//   * An empty partnerID passed to ListByCPAPartner returns an empty
//     slice so a misconfigured caller cannot enumerate every standalone
//     (non-CPA) key. Standalone keys carry CPAPartnerID = "".

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestAPIKeyStore_CreateWithCPAPartnerID(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	k := &domain.APIKey{
		ID:           "key_cpa_1",
		KeyHash:      "hash_cpa_1",
		Name:         "cpa/p1",
		CreatedBy:    "cpa-plugin",
		CPAPartnerID: "p1",
		Status:       "active",
		MonthlyQuota: 1000,
		MarkupPct:    1.2,
		CreatedAt:    time.Now().UTC(),
	}
	if err := store.Create(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.CPAPartnerID != "p1" {
		t.Errorf("CPAPartnerID: got %q want p1", got.CPAPartnerID)
	}
	if got.CreatedBy != "cpa-plugin" {
		t.Errorf("CreatedBy: got %q want cpa-plugin", got.CreatedBy)
	}
	if got.Name != "cpa/p1" {
		t.Errorf("Name: got %q want cpa/p1", got.Name)
	}
	if got.MonthlyQuota != 1000 {
		t.Errorf("MonthlyQuota: got %d want 1000", got.MonthlyQuota)
	}
}

func TestAPIKeyStore_ListByCPAPartner(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	// Two keys for p1, one for p2. Timestamps are spaced so ORDER BY
	// created_at DESC gives a deterministic newest-first order.
	base := time.Now().UTC()
	rows := []*domain.APIKey{
		{ID: "k_p1_a", KeyHash: "h1", Name: "p1-a", CPAPartnerID: "p1", CreatedBy: "cpa-plugin", CreatedAt: base},
		{ID: "k_p1_b", KeyHash: "h2", Name: "p1-b", CPAPartnerID: "p1", CreatedBy: "cpa-plugin", CreatedAt: base.Add(time.Second)},
		{ID: "k_p2_a", KeyHash: "h3", Name: "p2-a", CPAPartnerID: "p2", CreatedBy: "cpa-plugin", CreatedAt: base.Add(2 * time.Second)},
		// A standalone key with empty CPAPartnerID must not leak into
		// any partner-scoped listing.
		{ID: "k_standalone", KeyHash: "h4", Name: "standalone", CreatedBy: "admin", CreatedAt: base.Add(3 * time.Second)},
	}
	for _, k := range rows {
		if err := store.Create(ctx, k); err != nil {
			t.Fatalf("create %s: %v", k.ID, err)
		}
	}

	got, err := store.ListByCPAPartner(ctx, "p1")
	if err != nil {
		t.Fatalf("list p1: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("p1 list len: got %d want 2 (%+v)", len(got), got)
	}
	// Newest first: k_p1_b was created after k_p1_a.
	if got[0].ID != "k_p1_b" || got[1].ID != "k_p1_a" {
		t.Errorf("p1 order: got [%s, %s] want [k_p1_b, k_p1_a]", got[0].ID, got[1].ID)
	}
	for _, r := range got {
		if r.CPAPartnerID != "p1" {
			t.Errorf("stray row for p2/standalone in p1 list: %+v", r)
		}
	}

	got2, err := store.ListByCPAPartner(ctx, "p2")
	if err != nil {
		t.Fatalf("list p2: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != "k_p2_a" {
		t.Errorf("p2 list: got %+v want single k_p2_a", got2)
	}
}

func TestAPIKeyStore_ListByCPAPartnerEmpty(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	// Even with rows in the table, an empty partnerID must return an
	// empty slice — standalone keys (CPAPartnerID = "") are not part of
	// any partner's set and must not be enumerable through this path.
	if err := store.Create(ctx, &domain.APIKey{
		ID: "k_standalone_1", KeyHash: "h_x", Name: "admin",
		CreatedBy: "admin", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create standalone: %v", err)
	}
	if err := store.Create(ctx, &domain.APIKey{
		ID: "k_cpa_1", KeyHash: "h_y", Name: "cpa/p1",
		CreatedBy: "cpa-plugin", CPAPartnerID: "p1", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create cpa: %v", err)
	}

	got, err := store.ListByCPAPartner(ctx, "")
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty partnerID must return empty slice, got %d rows: %+v", len(got), got)
	}
}
