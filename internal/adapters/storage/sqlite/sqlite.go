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

// migrate applies embedded SQL files in numeric order. Each file's version
// is derived from its filename prefix (e.g. "001_init.sql" → 1). Applied
// versions are recorded in schema_versions to avoid re-running.
func (db *DB) migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	type step struct {
		version int
		name    string
		sql     string
	}
	var steps []step
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
		steps = append(steps, step{version: v, name: e.Name(), sql: string(body)})
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
		if _, err := db.ExecContext(ctx, s.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", s.name, err)
		}
	}
	return nil
}
