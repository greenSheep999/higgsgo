package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func openMem(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenAppliesMigrations(t *testing.T) {
	db := openMem(t)

	// schema_versions should carry every applied migration. The runner
	// walks migrations/*.sql in lexicographic order and inserts one row per
	// file that succeeded; the highest version must therefore match the
	// number of files on disk.
	var v int
	err := db.QueryRow(`SELECT version FROM schema_versions ORDER BY version DESC LIMIT 1`).Scan(&v)
	if err != nil {
		t.Fatalf("query schema_versions: %v", err)
	}
	if v < 6 {
		t.Fatalf("expected schema_versions.version >= 6, got %d (migration 006 not applied?)", v)
	}

	// accounts table should exist and be empty.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 accounts, got %d", count)
	}

	// The two columns added by migration 003 must be present on jobs.
	// pragma_table_info(name) returns one row per column.
	var cbCount, pbCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = 'callback_url'
	`).Scan(&cbCount); err != nil {
		t.Fatalf("query pragma_table_info (callback_url): %v", err)
	}
	if cbCount != 1 {
		t.Fatalf("jobs.callback_url column missing (migration 003 not applied)")
	}
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name = 'pre_balance_h'
	`).Scan(&pbCount); err != nil {
		t.Fatalf("query pragma_table_info (pre_balance_h): %v", err)
	}
	if pbCount != 1 {
		t.Fatalf("jobs.pre_balance_h column missing (migration 003 not applied)")
	}

	// The column added by migration 004 must be present on api_keys.
	var cpaCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('api_keys') WHERE name = 'cpa_partner_id'
	`).Scan(&cpaCount); err != nil {
		t.Fatalf("query pragma_table_info (cpa_partner_id): %v", err)
	}
	if cpaCount != 1 {
		t.Fatalf("api_keys.cpa_partner_id column missing (migration 004 not applied)")
	}

	// Migration 006 adds composite indexes for list / purge / usage hot
	// paths. Sanity check a couple by name; the migration_perf_test.go
	// suite covers each new index individually.
	for _, idx := range []string{
		"idx_jobs_api_key_request_ts",
		"idx_usage_api_key_ts",
	} {
		var idxCount int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?`, idx,
		).Scan(&idxCount); err != nil {
			t.Fatalf("query sqlite_master (%s): %v", idx, err)
		}
		if idxCount != 1 {
			t.Fatalf("index %s missing (migration 006 not applied)", idx)
		}
	}
}

func TestAccountStore_UpsertGetList(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	a := &domain.Account{
		ID:                  "user_test_1",
		Email:               "test@example.com",
		Password:            "encrypted",
		SessionID:           "sess_abc",
		CookiesJSON:         `{"foo":"bar"}`,
		UserAgent:           "Mozilla/5.0",
		DataDomeClientID:    "dd_client",
		WorkspaceID:         "ws_1",
		PlanType:            domain.PlanPlus,
		HasUnlim:            true,
		HasFlexUnlim:        true,
		SubscriptionBalance: 100000,
		TotalPlanCredits:    100000,
		Status:              domain.StatusActive,
		RegisteredAt:        time.Now().UTC(),
		ImportedAt:          time.Now().UTC(),
	}
	if err := store.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Email != a.Email {
		t.Errorf("email: got %q want %q", got.Email, a.Email)
	}
	if got.PlanType != domain.PlanPlus {
		t.Errorf("plan: got %s want plus", got.PlanType)
	}
	if !got.HasUnlim || !got.HasFlexUnlim {
		t.Errorf("unlim flags not preserved: %+v", got)
	}
	if got.SubscriptionBalance != 100000 {
		t.Errorf("balance: got %d want 100000", got.SubscriptionBalance)
	}

	list, err := store.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len: got %d want 1", len(list))
	}
}

func TestAccountStore_PickAndLock(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()

	// Two candidates: one starter, one plus.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(store.Upsert(ctx, &domain.Account{
		ID: "starter_1", Email: "s1@x.com", Password: "-", SessionID: "s", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanStarter, SubscriptionBalance: 20000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(store.Upsert(ctx, &domain.Account{
		ID: "plus_1", Email: "p1@x.com", Password: "-", SessionID: "s", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, HasUnlim: true, SubscriptionBalance: 100000, Status: domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))

	// RequiresPaid should skip starter.
	got, tok, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 1000, RequiresPaid: true,
	})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.ID != "plus_1" {
		t.Errorf("expected plus_1, got %s", got.ID)
	}
	if got.InFlightJobs != 1 {
		t.Errorf("in_flight after pick: got %d want 1", got.InFlightJobs)
	}

	// Unlock should decrement.
	if err := store.Unlock(ctx, got.ID, tok); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	after, _ := store.Get(ctx, got.ID)
	if after.InFlightJobs != 0 {
		t.Errorf("in_flight after unlock: got %d want 0", after.InFlightJobs)
	}

	// No eligible account when budget too high.
	_, _, err = store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 10_000_000, RequiresPaid: true,
	})
	if err != domain.ErrNoEligibleAccount {
		t.Errorf("expected ErrNoEligibleAccount, got %v", err)
	}
}
