package sqlite

// Tests for SettingsStore: Get/Set roundtrip, upsert semantics, and
// ErrSettingNotFound fallback. Backed by an in-memory SQLite handle
// via openMem.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestSettingsStore_GetMissingReturnsErrSettingNotFound(t *testing.T) {
	db := openMem(t)
	store := NewSettingsStore(db)

	_, err := store.Get(context.Background(), "admin_bearer")
	if !errors.Is(err, domain.ErrSettingNotFound) {
		t.Fatalf("expected ErrSettingNotFound, got %v", err)
	}
}

func TestSettingsStore_SetThenGet(t *testing.T) {
	db := openMem(t)
	store := NewSettingsStore(db)
	ctx := context.Background()

	if err := store.Set(ctx, "admin_bearer", "s3cret"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := store.Get(ctx, "admin_bearer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "s3cret" {
		t.Fatalf("got %q, want %q", got, "s3cret")
	}
}

func TestSettingsStore_SetIsUpsert(t *testing.T) {
	db := openMem(t)
	store := NewSettingsStore(db)
	ctx := context.Background()

	for _, v := range []string{"one", "two", "three"} {
		if err := store.Set(ctx, "admin_bearer", v); err != nil {
			t.Fatalf("set %q: %v", v, err)
		}
	}
	got, err := store.Get(ctx, "admin_bearer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "three" {
		t.Fatalf("expected upsert to leave last value, got %q", got)
	}
	// Table must still contain exactly one row for the key.
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM system_settings WHERE key = ?`, "admin_bearer",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one row after upsert, got %d", count)
	}
}

func TestSettingsStore_UpdatedAt(t *testing.T) {
	db := openMem(t)
	store := NewSettingsStore(db)
	ctx := context.Background()

	before := time.Now().UTC().Add(-time.Second)
	if err := store.Set(ctx, "admin_bearer", "abc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	ts, err := store.UpdatedAt(ctx, "admin_bearer")
	if err != nil {
		t.Fatalf("updated_at: %v", err)
	}
	if ts.Before(before) {
		t.Fatalf("updated_at %v earlier than %v", ts, before)
	}
	if _, err := store.UpdatedAt(ctx, "nonexistent"); !errors.Is(err, domain.ErrSettingNotFound) {
		t.Fatalf("expected ErrSettingNotFound for missing key, got %v", err)
	}
}
