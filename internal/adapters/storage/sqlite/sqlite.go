// Package sqlite provides a SQLite-backed implementation of the storage
// ports (AccountStore, JobStore, APIKeyStore, ...) declared in
// internal/ports. It uses modernc.org/sqlite (pure Go, no CGO).
//
// The connection is opened with WAL journal mode and foreign keys enabled.
// Migrations under ./migrations/ are embedded and applied on Open.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps a *sql.DB configured for SQLite. All store implementations share
// this handle.
type DB struct {
	*sql.DB
	path string
}

// Open initializes the SQLite database at the given path and applies all
// pending migrations. Passing ":memory:" opens an ephemeral in-memory DB
// (useful for tests). The caller must Close() when done.
func Open(ctx context.Context, path string) (*DB, error) {
	// modernc.org/sqlite driver name is "sqlite" (not sqlite3).
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open %q: %w", path, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	db := &DB{DB: sqlDB, path: path}
	if err := db.migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Path returns the DSN this DB was opened with.
func (db *DB) Path() string { return db.path }

// migrationStep is one embedded migration file resolved for execution.
// Hoisted out of migrate() to package scope so applyMigrationTx (below)
// can accept the value without redeclaring the type.
type migrationStep struct {
	version int
	name    string
	sql     string
}

// migrate applies embedded SQL files in numeric order. Each file's version
// is derived from its filename prefix (e.g. "001_init.sql" → 1). Applied
// versions are recorded in schema_versions to avoid re-running.
func (db *DB) migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var steps []migrationStep
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		parts := strings.SplitN(e.Name(), "_", 2)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		steps = append(steps, migrationStep{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].version < steps[j].version })

	// The very first migration creates schema_versions itself, so we must
	// tolerate the table not yet existing.
	applied := map[int]bool{}
	if rows, err := db.QueryContext(ctx, `SELECT version FROM schema_versions`); err == nil {
		for rows.Next() {
			var v int
			if err := rows.Scan(&v); err == nil {
				applied[v] = true
			}
		}
		_ = rows.Close()
	}

	for _, s := range steps {
		if applied[s.version] {
			continue
		}
		// Run each migration in its own transaction so any statement
		// failure rolls the whole file back atomically. This matters
		// for multi-statement migrations (e.g. 018 does a large
		// dedupe DELETE followed by CREATE UNIQUE INDEX — if the
		// index build fails partway, the DELETE must not persist).
		// SQLite auto-commits every statement outside a transaction,
		// so without this the DB could be left half-migrated.
		if err := applyMigrationTx(ctx, db.DB, s); err != nil {
			return err
		}
	}
	return nil
}

// applyMigrationTx wraps one migration file's execution in a BEGIN /
// COMMIT block plus the schema_versions bookkeeping. On any error the
// tx is rolled back so partial writes don't leak.
//
// Exception: files that contain PRAGMA statements bypass the tx
// wrapper. SQLite refuses "Safety level may not be changed inside a
// transaction" (and similar) when PRAGMA sits inside BEGIN/COMMIT,
// which would strand the whole boot on migration 001. Files that mix
// DDL + PRAGMA cannot get transactional atomicity — but 001 is the
// only such file and it runs on a fresh DB where partial application
// is not a real hazard. New migrations MUST NOT include PRAGMA
// statements; keep those in the Open() DSN string instead.
func applyMigrationTx(ctx context.Context, db *sql.DB, s migrationStep) error {
	if containsPragma(s.sql) {
		return applyMigrationNoTx(ctx, db, s)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx for %s: %w", s.name, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op if already committed
	if _, err := tx.ExecContext(ctx, s.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", s.name, err)
	}
	// Register the applied version so a subsequent Open won't try to
	// re-run the same DDL — previously we relied on individual
	// migration files to write their own schema_versions row, but
	// only 001 actually did that. Everything past 001 was replayed
	// every startup, which was silently harmless for CREATE INDEX
	// IF NOT EXISTS but blows up on ALTER TABLE ... ADD COLUMN.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (?, datetime('now'))`,
		s.version,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", s.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", s.name, err)
	}
	return nil
}

// applyMigrationNoTx runs a migration file that contains PRAGMA
// statements (currently 001 only) with auto-commit semantics. The
// schema_versions row is inserted separately so a rerun of the same
// version is idempotent on already-applied files.
func applyMigrationNoTx(ctx context.Context, db *sql.DB, s migrationStep) error {
	if _, err := db.ExecContext(ctx, s.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", s.name, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_versions (version, applied_at) VALUES (?, datetime('now'))`,
		s.version,
	); err != nil {
		return fmt.Errorf("record migration %s: %w", s.name, err)
	}
	return nil
}

// containsPragma returns true if the migration SQL contains a PRAGMA
// statement anywhere at start-of-line. Comment-only lines and leading
// whitespace are ignored. Case-insensitive so PRAGMA / pragma both
// match.
func containsPragma(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		if len(trimmed) >= 6 && strings.EqualFold(trimmed[:6], "PRAGMA") {
			return true
		}
	}
	return false
}
