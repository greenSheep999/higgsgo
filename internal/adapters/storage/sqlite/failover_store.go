package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// FailoverEventStore is a thin persistence layer over
// account_failover_events. All operations are single-statement so
// concurrent callers (the failover controller, admin reads, and the
// recoverer) do not need cross-store locking.
type FailoverEventStore struct {
	db *DB
}

// NewFailoverEventStore constructs a FailoverEventStore over db.
func NewFailoverEventStore(db *DB) *FailoverEventStore { return &FailoverEventStore{db: db} }

// Insert appends one event row. Errors from the underlying driver are
// wrapped so the failover controller's error path stays clean.
func (s *FailoverEventStore) Insert(ctx context.Context, accountID string, kind ports.FailoverEventKind, reason string, httpStatus int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO account_failover_events (account_id, kind, reason, http_status, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		accountID, string(kind), reason, httpStatus, fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("failover events: insert %s/%s: %w", accountID, kind, err)
	}
	return nil
}

// Count returns the number of (accountID, kind) events whose created_at
// falls within the last windowSec seconds. A windowSec <= 0 short-
// circuits to 0 so a misconfigured caller cannot accidentally count
// the whole table.
func (s *FailoverEventStore) Count(ctx context.Context, accountID string, kind ports.FailoverEventKind, windowSec int) (int, error) {
	if windowSec <= 0 {
		return 0, nil
	}
	since := time.Now().Add(-time.Duration(windowSec) * time.Second)
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM account_failover_events
		WHERE account_id = ? AND kind = ? AND created_at >= ?`,
		accountID, string(kind), fmtTime(since)).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return n, nil
}

// CountRecentDisables returns how many DISTINCT accounts were disabled
// via a consec_fail / evict event in the last windowSec seconds. Feeds
// the pool-level outage guard: when a higgsfield-wide incident spikes
// the count above the operator-configured threshold, the failover
// controller stops disabling accounts to avoid draining the pool over
// what turned out to be a global upstream failure.
func (s *FailoverEventStore) CountRecentDisables(ctx context.Context, windowSec int) (int, error) {
	if windowSec <= 0 {
		return 0, nil
	}
	since := time.Now().Add(-time.Duration(windowSec) * time.Second)
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT account_id)
		FROM account_failover_events
		WHERE reason IN ('consec_fail', 'evict') AND created_at >= ?`,
		fmtTime(since)).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return n, nil
}

// List returns the newest `limit` events for the given account. limit
// <= 0 is clamped to a small default so a misconfigured caller cannot
// dump the whole audit trail.
func (s *FailoverEventStore) List(ctx context.Context, accountID string, limit int) ([]ports.FailoverEventRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, account_id, kind, reason, http_status, created_at
		FROM account_failover_events
		WHERE account_id = ?
		ORDER BY created_at DESC
		LIMIT ?`,
		accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ports.FailoverEventRow
	for rows.Next() {
		var (
			r         ports.FailoverEventRow
			kindStr   string
			createdAt sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.AccountID, &kindStr, &r.Reason, &r.HTTPStatus, &createdAt); err != nil {
			return nil, err
		}
		r.Kind = ports.FailoverEventKind(kindStr)
		r.CreatedAt = parseTime(createdAt.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteForAccount removes every event row for accountID. Called by
// the operator "recover disabled" flow so the account starts with a
// clean judge / evict window when it re-enters rotation.
func (s *FailoverEventStore) DeleteForAccount(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM account_failover_events WHERE account_id = ?`, accountID)
	return err
}

// FailoverOverridesStore persists per-account tuning overrides.
type FailoverOverridesStore struct {
	db *DB
}

// NewFailoverOverridesStore constructs a FailoverOverridesStore.
func NewFailoverOverridesStore(db *DB) *FailoverOverridesStore {
	return &FailoverOverridesStore{db: db}
}

// Get returns the override row for accountID or nil if none. A nil
// return is not an error — the failover controller treats "no row" as
// "inherit every global default".
func (s *FailoverOverridesStore) Get(ctx context.Context, accountID string) (*ports.FailoverOverride, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT enabled, fail_limit, judge_window_sec, judge_count,
		       cooldown_sec, evict_window_sec, evict_count, updated_at
		FROM account_failover_overrides
		WHERE account_id = ?`, accountID)
	var (
		enabled        sql.NullInt64
		failLimit      sql.NullInt64
		judgeWindowSec sql.NullInt64
		judgeCount     sql.NullInt64
		cooldownSec    sql.NullInt64
		evictWindowSec sql.NullInt64
		evictCount     sql.NullInt64
		updatedAt      sql.NullString
	)
	if err := row.Scan(&enabled, &failLimit, &judgeWindowSec, &judgeCount,
		&cooldownSec, &evictWindowSec, &evictCount, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	o := &ports.FailoverOverride{
		AccountID: accountID,
		UpdatedAt: parseTime(updatedAt.String),
	}
	if enabled.Valid {
		b := enabled.Int64 != 0
		o.Enabled = &b
	}
	if failLimit.Valid {
		v := int(failLimit.Int64)
		o.FailLimit = &v
	}
	if judgeWindowSec.Valid {
		v := int(judgeWindowSec.Int64)
		o.JudgeWindowSec = &v
	}
	if judgeCount.Valid {
		v := int(judgeCount.Int64)
		o.JudgeCount = &v
	}
	if cooldownSec.Valid {
		v := int(cooldownSec.Int64)
		o.CooldownSec = &v
	}
	if evictWindowSec.Valid {
		v := int(evictWindowSec.Int64)
		o.EvictWindowSec = &v
	}
	if evictCount.Valid {
		v := int(evictCount.Int64)
		o.EvictCount = &v
	}
	return o, nil
}

// Upsert writes (or replaces) the override row for o.AccountID.
// Pointer fields land as NULL when nil so the controller's merge helper
// falls back to global defaults on read.
func (s *FailoverOverridesStore) Upsert(ctx context.Context, o *ports.FailoverOverride) error {
	if o == nil || o.AccountID == "" {
		return fmt.Errorf("failover overrides: account_id required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO account_failover_overrides (
			account_id, enabled, fail_limit,
			judge_window_sec, judge_count, cooldown_sec,
			evict_window_sec, evict_count, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			enabled = excluded.enabled,
			fail_limit = excluded.fail_limit,
			judge_window_sec = excluded.judge_window_sec,
			judge_count = excluded.judge_count,
			cooldown_sec = excluded.cooldown_sec,
			evict_window_sec = excluded.evict_window_sec,
			evict_count = excluded.evict_count,
			updated_at = excluded.updated_at`,
		o.AccountID,
		nullableIntPtr(o.Enabled),
		nullableIntPtrValue(o.FailLimit),
		nullableIntPtrValue(o.JudgeWindowSec),
		nullableIntPtrValue(o.JudgeCount),
		nullableIntPtrValue(o.CooldownSec),
		nullableIntPtrValue(o.EvictWindowSec),
		nullableIntPtrValue(o.EvictCount),
		fmtTime(time.Now()),
	)
	return err
}

// Delete removes the override row for accountID. A missing row is a
// no-op so callers can idempotently reset.
func (s *FailoverOverridesStore) Delete(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM account_failover_overrides WHERE account_id = ?`, accountID)
	return err
}

// nullableIntPtr converts a *bool into an INTEGER-compatible any that
// stores NULL for a nil pointer and 0/1 otherwise.
func nullableIntPtr(b *bool) any {
	if b == nil {
		return nil
	}
	if *b {
		return 1
	}
	return 0
}

// nullableIntPtrValue is the *int counterpart to nullableIntPtr.
func nullableIntPtrValue(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
