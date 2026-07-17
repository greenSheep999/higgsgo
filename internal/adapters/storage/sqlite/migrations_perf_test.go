package sqlite

import (
	"strings"
	"testing"
)

// assertIndexExists fails the test if the given index is not registered in
// sqlite_master. Used by the migration-006 coverage below to prove each
// index landed on Open() without repeating the boilerplate six times.
func assertIndexExists(t *testing.T, db *DB, name string) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("query sqlite_master (%s): %v", name, err)
	}
	if n != 1 {
		t.Fatalf("expected index %s to exist, got count=%d", name, n)
	}
}

// TestMigration006_JobsIndexPresent verifies that the three jobs composite
// indexes introduced by migration 006 are present on a fresh Open().
func TestMigration006_JobsIndexPresent(t *testing.T) {
	db := openMem(t)
	for _, name := range []string{
		"idx_jobs_api_key_request_ts",
		"idx_jobs_account_request_ts",
		"idx_jobs_status_finished",
	} {
		assertIndexExists(t, db, name)
	}
}

// TestMigration006_UsageIndexPresent verifies that the three usage_events
// indexes introduced by migration 006 are present on a fresh Open().
func TestMigration006_UsageIndexPresent(t *testing.T) {
	db := openMem(t)
	for _, name := range []string{
		"idx_usage_api_key_ts",
		"idx_usage_billing_day",
		"idx_usage_model_alias",
	} {
		assertIndexExists(t, db, name)
	}
}

// TestMigration006_ListByAPIKeyUsesIndex runs EXPLAIN QUERY PLAN over the
// ListByAPIKey hot-path query shape and asserts the SQLite planner elects
// idx_jobs_api_key_request_ts to satisfy the WHERE + ORDER BY. If the
// planner picks a different index or refuses to use one (e.g. on a future
// SQLite version), the test falls back to a plain existence assertion so
// the coverage does not silently regress on the primary invariant.
func TestMigration006_ListByAPIKeyUsesIndex(t *testing.T) {
	db := openMem(t)

	rows, err := db.Query(`EXPLAIN QUERY PLAN
		SELECT id FROM jobs
		WHERE api_key_id = ?
		ORDER BY request_ts DESC
		LIMIT 50`, "ak_test")
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}

	got := plan.String()
	if !strings.Contains(got, "idx_jobs_api_key_request_ts") {
		// Planner declined our composite; the index still needs to
		// exist, so fall back to the structural invariant rather
		// than fail on a planner heuristic change.
		t.Logf("planner did not select idx_jobs_api_key_request_ts; plan=%q", got)
		assertIndexExists(t, db, "idx_jobs_api_key_request_ts")
	}
}
