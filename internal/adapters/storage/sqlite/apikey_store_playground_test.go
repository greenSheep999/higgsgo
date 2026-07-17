package sqlite

// Tests for the api_keys.playground_scope column added by migration 009
// and the UpdatePlaygroundScope write path. These verify:
//
//   * A key written with a non-empty PlaygroundScope round-trips through
//     Create -> Get with the value preserved.
//   * A key written without an explicit PlaygroundScope defaults to
//     PlaygroundScopeNone (matching the migration default).
//   * UpdatePlaygroundScope replaces the column and Get sees the new value.
//   * Unknown scope strings are normalised to PlaygroundScopeNone so a
//     malformed write cannot silently open access.
//   * UpdatePlaygroundScope on a missing id returns ErrAPIKeyNotFound.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestAPIKeyStore_CreateWithPlaygroundScope(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	k := &domain.APIKey{
		ID:              "key_pg_1",
		KeyHash:         "hash_pg_1",
		Name:            "pg/cheap",
		CreatedAt:       time.Now().UTC(),
		PlaygroundScope: domain.PlaygroundScopeCheap,
	}
	if err := store.Create(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PlaygroundScope != domain.PlaygroundScopeCheap {
		t.Errorf("PlaygroundScope: got %q want %q",
			got.PlaygroundScope, domain.PlaygroundScopeCheap)
	}
}

func TestAPIKeyStore_CreateDefaultPlaygroundScopeNone(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	k := &domain.APIKey{
		ID:        "key_default",
		KeyHash:   "hash_default",
		Name:      "default",
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Create(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PlaygroundScope != domain.PlaygroundScopeNone {
		t.Errorf("PlaygroundScope default: got %q want %q",
			got.PlaygroundScope, domain.PlaygroundScopeNone)
	}
}

func TestAPIKeyStore_UpdatePlaygroundScopeRoundtrip(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	k := &domain.APIKey{
		ID:        "key_upd",
		KeyHash:   "hash_upd",
		Name:      "upd",
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Create(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.UpdatePlaygroundScope(ctx, k.ID, domain.PlaygroundScopeFull); err != nil {
		t.Fatalf("update to full: %v", err)
	}
	got, err := store.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PlaygroundScope != domain.PlaygroundScopeFull {
		t.Errorf("after update to full: got %q want %q",
			got.PlaygroundScope, domain.PlaygroundScopeFull)
	}

	// Flipping back to none must clear the flag.
	if err := store.UpdatePlaygroundScope(ctx, k.ID, domain.PlaygroundScopeNone); err != nil {
		t.Fatalf("update to none: %v", err)
	}
	got, err = store.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PlaygroundScope != domain.PlaygroundScopeNone {
		t.Errorf("after update to none: got %q want %q",
			got.PlaygroundScope, domain.PlaygroundScopeNone)
	}
}

func TestAPIKeyStore_UpdatePlaygroundScopeUnknownNormalised(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	k := &domain.APIKey{
		ID:              "key_unknown",
		KeyHash:         "hash_unknown",
		Name:            "unknown",
		CreatedAt:       time.Now().UTC(),
		PlaygroundScope: domain.PlaygroundScopeFull,
	}
	if err := store.Create(ctx, k); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Passing a bogus scope must fall through to PlaygroundScopeNone
	// (deny-by-default) rather than persist a value the middleware can't
	// interpret.
	if err := store.UpdatePlaygroundScope(ctx, k.ID, domain.PlaygroundScope("bogus")); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := store.Get(ctx, k.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PlaygroundScope != domain.PlaygroundScopeNone {
		t.Errorf("unknown scope: got %q want %q",
			got.PlaygroundScope, domain.PlaygroundScopeNone)
	}
}

func TestAPIKeyStore_UpdatePlaygroundScopeMissing(t *testing.T) {
	db := openMem(t)
	store := NewAPIKeyStore(db)
	ctx := context.Background()

	err := store.UpdatePlaygroundScope(ctx, "ghost", domain.PlaygroundScopeFull)
	if err != domain.ErrAPIKeyNotFound {
		t.Fatalf("expected ErrAPIKeyNotFound, got %v", err)
	}
}
