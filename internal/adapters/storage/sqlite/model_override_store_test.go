package sqlite

// Tests for ModelOverrideStore: Get/Upsert roundtrip, three-state
// pointer semantics, ExtraAliases JSON encoding, Delete idempotency,
// and List ordering.

import (
	"context"
	"reflect"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestModelOverrideStore_GetMissingReturnsNil(t *testing.T) {
	db := openMem(t)
	store := NewModelOverrideStore(db)

	got, err := store.Get(context.Background(), "ghost-alias")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil override, got %+v", got)
	}
}

func TestModelOverrideStore_UpsertThenGet_PreservesPointers(t *testing.T) {
	db := openMem(t)
	store := NewModelOverrideStore(db)
	ctx := context.Background()

	// Nil StarterLocked = "inherit"; set RequiresUltra = "explicit true".
	locked := false
	ultra := true
	minCr := int64(50_000)
	in := &domain.ModelOverride{
		Alias:                "nano-banana-2",
		StarterLocked:        &locked,
		RequiresUltra:        &ultra,
		MinCreditsHundredths: &minCr,
		ExtraAliases:         []string{"banana-2-plus", "google-nano-banana-2"},
		Note:                 "downstream new-api dual-registration",
	}
	if err := store.Upsert(ctx, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.Get(ctx, "nano-banana-2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil override after upsert")
	}
	if got.StarterLocked == nil || *got.StarterLocked != false {
		t.Errorf("starter_locked: got %v, want ptr(false)", got.StarterLocked)
	}
	if got.RequiresPaid != nil {
		t.Errorf("requires_paid: got %v, want nil (unset)", got.RequiresPaid)
	}
	if got.RequiresUltra == nil || *got.RequiresUltra != true {
		t.Errorf("requires_ultra: got %v, want ptr(true)", got.RequiresUltra)
	}
	if got.MinCreditsHundredths == nil || *got.MinCreditsHundredths != 50_000 {
		t.Errorf("min_credits_hundredths: got %v, want ptr(50000)", got.MinCreditsHundredths)
	}
	wantAliases := []string{"banana-2-plus", "google-nano-banana-2"}
	if !reflect.DeepEqual(got.ExtraAliases, wantAliases) {
		t.Errorf("extra_aliases: got %v, want %v", got.ExtraAliases, wantAliases)
	}
	if got.Note != in.Note {
		t.Errorf("note: got %q, want %q", got.Note, in.Note)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updated_at should be populated on Get")
	}
}

func TestModelOverrideStore_UpsertReplacesRow(t *testing.T) {
	db := openMem(t)
	store := NewModelOverrideStore(db)
	ctx := context.Background()

	paid := true
	if err := store.Upsert(ctx, &domain.ModelOverride{Alias: "x", RequiresPaid: &paid}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	// Second write clears requires_paid, sets requires_ultra.
	ultra := true
	if err := store.Upsert(ctx, &domain.ModelOverride{Alias: "x", RequiresUltra: &ultra}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err := store.Get(ctx, "x")
	if err != nil || got == nil {
		t.Fatalf("get: %v %+v", err, got)
	}
	if got.RequiresPaid != nil {
		t.Errorf("requires_paid should be NULL after second upsert, got %v", got.RequiresPaid)
	}
	if got.RequiresUltra == nil || *got.RequiresUltra != true {
		t.Errorf("requires_ultra: got %v", got.RequiresUltra)
	}
}

func TestModelOverrideStore_ExtraAliases_EmptyRoundTrip(t *testing.T) {
	db := openMem(t)
	store := NewModelOverrideStore(db)
	ctx := context.Background()

	if err := store.Upsert(ctx, &domain.ModelOverride{Alias: "y", Note: "just a note"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := store.Get(ctx, "y")
	if err != nil || got == nil {
		t.Fatalf("get: %v %+v", err, got)
	}
	if len(got.ExtraAliases) != 0 {
		t.Errorf("extra_aliases: expected empty, got %v", got.ExtraAliases)
	}
}

func TestModelOverrideStore_DeleteIdempotent(t *testing.T) {
	db := openMem(t)
	store := NewModelOverrideStore(db)
	ctx := context.Background()

	if err := store.Delete(ctx, "no-such-alias"); err != nil {
		t.Errorf("delete missing row: %v", err)
	}
	paid := true
	if err := store.Upsert(ctx, &domain.ModelOverride{Alias: "z", RequiresPaid: &paid}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.Delete(ctx, "z"); err != nil {
		t.Errorf("delete existing row: %v", err)
	}
	got, err := store.Get(ctx, "z")
	if err != nil || got != nil {
		t.Errorf("after delete: got %+v %v, want (nil, nil)", got, err)
	}
}

func TestModelOverrideStore_ListReturnsAll(t *testing.T) {
	db := openMem(t)
	store := NewModelOverrideStore(db)
	ctx := context.Background()

	for _, a := range []string{"a", "b", "c"} {
		if err := store.Upsert(ctx, &domain.ModelOverride{
			Alias:        a,
			ExtraAliases: []string{a + "-alt"},
		}); err != nil {
			t.Fatalf("upsert %s: %v", a, err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(list))
	}
	// Every row should carry its extra alias.
	for _, o := range list {
		if len(o.ExtraAliases) != 1 || o.ExtraAliases[0] != o.Alias+"-alt" {
			t.Errorf("row %s extra_aliases: %v", o.Alias, o.ExtraAliases)
		}
	}
}
