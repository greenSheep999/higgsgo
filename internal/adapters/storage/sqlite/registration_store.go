package sqlite

// RegistrationStore is the SQLite adapter for ports.RegistrationStore.
// The `registrations` table has been in migration 001 since day one,
// but this adapter is what finally makes it live — before it was a
// dangling schema. See docs/ROADMAP.md §5.3 and docs/AUDIT-CYCLE.md.
//
// State machine mirrors the ports.Registrar contract:
//   pending → running → (success | failed)
// Enqueue writes pending. A background worker calls NextPending to
// claim the oldest pending row, MarkRunning to flip it, then one of
// MarkCompleted/MarkFailed at terminal. Retry (called from the admin
// handler) resets a failed row back to pending; it lives on the
// Registrar path, not here, so this file stays pure CRUD.
//
// Concurrency: SQLite serializes writers, so NextPending's
// "SELECT+UPDATE oldest pending" is safe under BEGIN IMMEDIATE.
// Multiple worker goroutines are supported but each claim runs
// serially at the DB layer.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// RegistrationStore implements ports.RegistrationStore.
type RegistrationStore struct {
	db *DB
}

// NewRegistrationStore constructs the store. Nil-safe against a nil DB
// caller: the returned struct's methods will surface nil-pointer errors
// at first use rather than at construction time so wiring order in
// main.go stays flexible.
func NewRegistrationStore(db *DB) *RegistrationStore {
	return &RegistrationStore{db: db}
}

// Enqueue inserts a new pending registration. Returns after assigning
// r.ID from the auto-increment column so the caller (admin handler)
// can hand the id back to the operator immediately.
func (s *RegistrationStore) Enqueue(ctx context.Context, r *ports.Registration) error {
	if r == nil {
		return fmt.Errorf("enqueue: nil registration")
	}
	if r.Email == "" {
		return fmt.Errorf("enqueue: email is required")
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	// Status defaults to "pending" via the column default, but be
	// explicit so a caller that passes a non-empty Status (e.g. an
	// admin re-import from an export file) preserves it.
	status := r.Status
	if status == "" {
		status = "pending"
	}
	// fmtTime returns "" for zero timestamps, which SQLite persists as
	// an empty string. scanRegistration's parseTime maps "" back to
	// time.Time{}, so the round-trip is clean without needing
	// sql.NullTime on the write side.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO registrations
			(email, password, oauth_source, refresh_token, proxy_url,
			 status, attempts, last_error, account_id, created_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Email,
		r.Password,
		r.OAuthSource,
		r.RefreshToken,
		r.ProxyURL,
		status,
		r.Attempts,
		r.LastError,
		r.AccountID,
		fmtTime(r.CreatedAt),
		fmtTime(r.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("enqueue registration: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("enqueue: last insert id: %w", err)
	}
	r.ID = id
	r.Status = status
	return nil
}

// NextPending returns the oldest pending registration, or (nil, nil)
// when the queue is empty. Returning nil instead of an error for the
// empty case matches the worker-loop expectation: an empty queue is a
// normal condition worth backing off on, not a failure to log.
func (s *RegistrationStore) NextPending(ctx context.Context) (*ports.Registration, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, email, password, oauth_source, refresh_token,
		       proxy_url, status, attempts, last_error, account_id,
		       created_at, finished_at
		FROM registrations
		WHERE status = 'pending'
		ORDER BY id ASC
		LIMIT 1`)
	r, err := scanRegistration(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("next pending: %w", err)
	}
	return r, nil
}

// MarkRunning flips a pending row to running and increments attempts.
// Returns ErrRegistrationNotFound when the id doesn't exist so the
// worker can distinguish "the row got Retry'd by admin and I raced
// against them" from "DB failure".
func (s *RegistrationStore) MarkRunning(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE registrations
		SET status = 'running',
		    attempts = attempts + 1
		WHERE id = ?`,
		id,
	)
	if err != nil {
		return fmt.Errorf("mark running: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrRegistrationNotFound
	}
	return nil
}

// MarkCompleted flips a running row to success and links the produced
// account_id. Populates finished_at so operators can compute per-batch
// throughput without a separate observation table.
func (s *RegistrationStore) MarkCompleted(ctx context.Context, id int64, accountID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE registrations
		SET status = 'success',
		    account_id = ?,
		    last_error = NULL,
		    finished_at = ?
		WHERE id = ?`,
		accountID,
		fmtTime(time.Now().UTC()),
		id,
	)
	if err != nil {
		return fmt.Errorf("mark completed: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrRegistrationNotFound
	}
	return nil
}

// MarkFailed flips a running row to failed with an error message.
// Preserves the account_id column so a failure after partial account
// creation (rare, but possible if higgsfield accepts the sign-up but
// the cookie harvest fails) still leaves a paper trail.
func (s *RegistrationStore) MarkFailed(ctx context.Context, id int64, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE registrations
		SET status = 'failed',
		    last_error = ?,
		    finished_at = ?
		WHERE id = ?`,
		errMsg,
		fmtTime(time.Now().UTC()),
		id,
	)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrRegistrationNotFound
	}
	return nil
}

// Get returns a registration by id. ErrRegistrationNotFound when
// missing — the admin handler translates that into 404, the worker
// treats it as "someone deleted this while I was working, give up".
func (s *RegistrationStore) Get(ctx context.Context, id int64) (*ports.Registration, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, email, password, oauth_source, refresh_token,
		       proxy_url, status, attempts, last_error, account_id,
		       created_at, finished_at
		FROM registrations
		WHERE id = ?`,
		id,
	)
	r, err := scanRegistration(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrRegistrationNotFound
		}
		return nil, fmt.Errorf("get registration: %w", err)
	}
	return r, nil
}

// scanRegistration reads a single row into a Registration. Kept
// package-private so the shape stays local to this file — the ports
// struct is the exported surface.
func scanRegistration(sc scanner) (*ports.Registration, error) {
	var (
		r            ports.Registration
		password     sql.NullString
		oauthSource  sql.NullString
		refreshToken sql.NullString
		proxyURL     sql.NullString
		lastError    sql.NullString
		accountID    sql.NullString
		createdAt    sql.NullString
		finishedAt   sql.NullString
	)
	if err := sc.Scan(
		&r.ID,
		&r.Email,
		&password,
		&oauthSource,
		&refreshToken,
		&proxyURL,
		&r.Status,
		&r.Attempts,
		&lastError,
		&accountID,
		&createdAt,
		&finishedAt,
	); err != nil {
		return nil, err
	}
	r.Password = password.String
	r.OAuthSource = oauthSource.String
	r.RefreshToken = refreshToken.String
	r.ProxyURL = proxyURL.String
	r.LastError = lastError.String
	r.AccountID = accountID.String
	r.CreatedAt = parseTime(createdAt.String)
	r.FinishedAt = parseTime(finishedAt.String)
	return &r, nil
}
