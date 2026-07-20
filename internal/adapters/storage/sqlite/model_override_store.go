package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// ModelOverrideStore persists ports.ModelOverrideStore behind the
// model_overrides table (migration 015). Each row is one operator
// override on top of the static verified-models.json entry; nil
// pointer columns land as NULL so the registry merge helper falls
// back to spec defaults.
type ModelOverrideStore struct {
	db *DB
}

// NewModelOverrideStore builds a store over db.
func NewModelOverrideStore(db *DB) *ModelOverrideStore {
	return &ModelOverrideStore{db: db}
}

// Get returns the override for alias, or (nil, nil) if none.
func (s *ModelOverrideStore) Get(ctx context.Context, alias string) (*domain.ModelOverride, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT alias, starter_locked, requires_paid, requires_ultra, requires_unlim,
		       min_credits_hundredths, extra_aliases_json, note, updated_at
		FROM model_overrides
		WHERE alias = ?`, alias)
	return scanOverride(row)
}

// Upsert writes (or replaces) the override row for o.Alias. Every
// pointer / slice field lands as NULL / ” when nil/empty so the
// merge helper falls back to spec defaults.
func (s *ModelOverrideStore) Upsert(ctx context.Context, o *domain.ModelOverride) error {
	if o == nil || o.Alias == "" {
		return fmt.Errorf("model overrides: alias required")
	}
	extraJSON, err := marshalExtraAliases(o.ExtraAliases)
	if err != nil {
		return fmt.Errorf("marshal extra_aliases: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO model_overrides (
			alias, starter_locked, requires_paid, requires_ultra, requires_unlim,
			min_credits_hundredths, extra_aliases_json, note, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(alias) DO UPDATE SET
			starter_locked         = excluded.starter_locked,
			requires_paid          = excluded.requires_paid,
			requires_ultra         = excluded.requires_ultra,
			requires_unlim         = excluded.requires_unlim,
			min_credits_hundredths = excluded.min_credits_hundredths,
			extra_aliases_json     = excluded.extra_aliases_json,
			note                   = excluded.note,
			updated_at             = excluded.updated_at`,
		o.Alias,
		nullableBoolPtr(o.StarterLocked),
		nullableBoolPtr(o.RequiresPaid),
		nullableBoolPtr(o.RequiresUltra),
		nullableBoolPtr(o.RequiresUnlim),
		nullableInt64Ptr(o.MinCreditsHundredths),
		extraJSON,
		o.Note,
		fmtTime(time.Now().UTC()),
	)
	if err != nil {
		return fmt.Errorf("upsert model_overrides %q: %w", o.Alias, err)
	}
	return nil
}

// Delete removes the override for alias. Missing rows are a no-op.
func (s *ModelOverrideStore) Delete(ctx context.Context, alias string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_overrides WHERE alias = ?`, alias)
	if err != nil {
		return fmt.Errorf("delete model_overrides %q: %w", alias, err)
	}
	return nil
}

// List returns every override row, newest updated first.
func (s *ModelOverrideStore) List(ctx context.Context) ([]domain.ModelOverride, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT alias, starter_locked, requires_paid, requires_ultra, requires_unlim,
		       min_credits_hundredths, extra_aliases_json, note, updated_at
		FROM model_overrides
		ORDER BY updated_at DESC, alias ASC`)
	if err != nil {
		return nil, fmt.Errorf("list model_overrides: %w", err)
	}
	defer rows.Close()
	var out []domain.ModelOverride
	for rows.Next() {
		o, err := scanOverride(rows)
		if err != nil {
			return nil, err
		}
		if o != nil {
			out = append(out, *o)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// rowScanner is the common interface implemented by *sql.Row and
// *sql.Rows so scanOverride can serve both Get and List.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanOverride decodes one model_overrides row. Returns (nil, nil)
// for sql.ErrNoRows so the caller can treat "no row" as
// "spec defaults apply".
func scanOverride(sc rowScanner) (*domain.ModelOverride, error) {
	var (
		alias, note          string
		starter, paid        sql.NullInt64
		ultra, unlim         sql.NullInt64
		minCredits           sql.NullInt64
		extraJSON, updatedAt sql.NullString
	)
	err := sc.Scan(&alias, &starter, &paid, &ultra, &unlim, &minCredits, &extraJSON, &note, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	o := &domain.ModelOverride{
		Alias:        alias,
		Note:         note,
		ExtraAliases: unmarshalExtraAliases(extraJSON.String),
	}
	if starter.Valid {
		b := starter.Int64 != 0
		o.StarterLocked = &b
	}
	if paid.Valid {
		b := paid.Int64 != 0
		o.RequiresPaid = &b
	}
	if ultra.Valid {
		b := ultra.Int64 != 0
		o.RequiresUltra = &b
	}
	if unlim.Valid {
		b := unlim.Int64 != 0
		o.RequiresUnlim = &b
	}
	if minCredits.Valid {
		v := minCredits.Int64
		o.MinCreditsHundredths = &v
	}
	if updatedAt.Valid {
		o.UpdatedAt = parseTime(updatedAt.String)
	}
	return o, nil
}

// marshalExtraAliases produces the JSON string stored in
// extra_aliases_json. Empty slices are stored as NULL so a NULL check
// suffices to detect "no expansion" without parsing the JSON.
func marshalExtraAliases(aliases []string) (any, error) {
	if len(aliases) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(aliases)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

// unmarshalExtraAliases decodes extra_aliases_json. Empty / invalid
// content becomes nil — the merge helper treats that as "no
// expansion" without erroring.
func unmarshalExtraAliases(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// nullableBoolPtr converts a *bool into an INTEGER-compatible any
// that stores NULL for a nil pointer and 0/1 otherwise.
func nullableBoolPtr(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

// nullableInt64Ptr converts a *int64 into a driver-friendly any that
// stores NULL for nil and the int64 value otherwise.
func nullableInt64Ptr(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// verify at compile time that we satisfy the port.
var _ ports.ModelOverrideStore = (*ModelOverrideStore)(nil)
