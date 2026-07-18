package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// SettingsStore implements ports.SettingsStore backed by the
// system_settings table added in migration 014. See the migration
// file for the threat model — values are stored as raw strings, no
// hashing, because the only caller today is the admin bearer flow
// which is already plaintext in configs/*.toml.
type SettingsStore struct {
	db *DB
}

// NewSettingsStore returns a SettingsStore rooted at the given DB.
func NewSettingsStore(db *DB) *SettingsStore { return &SettingsStore{db: db} }

// Get returns the value stored under key. Returns
// domain.ErrSettingNotFound when no row exists.
func (s *SettingsStore) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM system_settings WHERE key = ?`, key,
	).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", domain.ErrSettingNotFound
	}
	if err != nil {
		return "", fmt.Errorf("select system_settings %q: %w", key, err)
	}
	return v, nil
}

// Set writes value under key, replacing any prior row and stamping
// updated_at with the current UTC wall-clock time.
func (s *SettingsStore) Set(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO system_settings (key, value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
		  value = excluded.value,
		  updated_at = excluded.updated_at`,
		key, value, fmtTime(time.Now().UTC()),
	)
	if err != nil {
		return fmt.Errorf("upsert system_settings %q: %w", key, err)
	}
	return nil
}

// UpdatedAt returns the wall-clock time the row under key was last
// written. Returns domain.ErrSettingNotFound when no row exists.
func (s *SettingsStore) UpdatedAt(ctx context.Context, key string) (time.Time, error) {
	var ts string
	err := s.db.QueryRowContext(ctx,
		`SELECT updated_at FROM system_settings WHERE key = ?`, key,
	).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, domain.ErrSettingNotFound
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("select updated_at %q: %w", key, err)
	}
	return parseTime(ts), nil
}
