package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// APIKeyStore implements ports.APIKeyStore backed by SQLite.
type APIKeyStore struct {
	db *DB
}

// NewAPIKeyStore returns a fresh APIKeyStore.
func NewAPIKeyStore(db *DB) *APIKeyStore { return &APIKeyStore{db: db} }

// apiKeyColumns lists the columns scanAPIKey expects in order. Kept as
// a constant so every SELECT stays in sync with the scanner.
const apiKeyColumns = `id, key_hash, name, created_by, cpa_partner_id, group_id, status,
	monthly_quota, monthly_used, markup_pct,
	created_at, last_used_at`

// Get fetches a key by id.
func (s *APIKeyStore) Get(ctx context.Context, id string) (*domain.APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id)
	return scanAPIKey(row)
}

// GetByHash looks up a key by its bcrypt hash. Used by the auth middleware
// on every /v1 request.
func (s *APIKeyStore) GetByHash(ctx context.Context, keyHash string) (*domain.APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = ?`, keyHash)
	return scanAPIKey(row)
}

// Create inserts a new key. Caller supplies the id, key_hash, name, and
// (optional) quota/markup. CPAPartnerID is only set for keys minted via
// the /internal/register CPA plugin route.
func (s *APIKeyStore) Create(ctx context.Context, k *domain.APIKey) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (
			id, key_hash, name, created_by, cpa_partner_id, group_id, status,
			monthly_quota, monthly_used, markup_pct,
			created_at, last_used_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.KeyHash, k.Name, nullStr(k.CreatedBy), k.CPAPartnerID, k.GroupID, defaultStatus(k.Status),
		k.MonthlyQuota, k.MonthlyUsed, defaultMarkup(k.MarkupPct),
		fmtTime(defaultTime(k.CreatedAt)), fmtTime(k.LastUsedAt),
	)
	if err != nil {
		return fmt.Errorf("insert api key %s: %w", k.ID, err)
	}
	return nil
}

// Revoke marks a key inactive. The row is kept for audit.
func (s *APIKeyStore) Revoke(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET status = 'revoked' WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAPIKeyNotFound
	}
	return nil
}

// IncrementUsage atomically adds the charged amount to monthly_used and
// updates last_used_at. Caller enforces quota beforehand.
func (s *APIKeyStore) IncrementUsage(ctx context.Context, id string, chargedHundredths int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET monthly_used = monthly_used + ?, last_used_at = ?
		WHERE id = ?`,
		chargedHundredths, fmtTime(time.Now()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAPIKeyNotFound
	}
	return nil
}

// Rotate mints a fresh (plaintext, hash) pair via the apikey package and
// swaps only the key_hash column of the row. Every other column — name,
// quota, markup, CPA partner, group binding, usage — is preserved so a
// rotation never invalidates existing routing / accounting state. The
// plaintext is returned to the caller so the admin handler can expose it
// to the operator exactly once.
func (s *APIKeyStore) Rotate(ctx context.Context, id string) (string, error) {
	plaintext, hash, err := apikey.Generate()
	if err != nil {
		return "", fmt.Errorf("rotate api key %s: mint: %w", id, err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET key_hash = ?, updated_at = ?
		WHERE id = ?`,
		hash, fmtTime(time.Now().UTC()), id)
	if err != nil {
		return "", fmt.Errorf("rotate api key %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", domain.ErrAPIKeyNotFound
	}
	return plaintext, nil
}

// Pause flips status from "active" to "paused". Only the active -> paused
// transition mutates the row. If the current status is "revoked" the call
// returns ErrAPIKeyRevoked (revoked is terminal); if it is already paused
// the call is a no-op that returns nil.
func (s *APIKeyStore) Pause(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		domain.APIKeyStatusPaused, fmtTime(time.Now().UTC()), id, domain.APIKeyStatusActive)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}
	return s.explainInactiveTransition(ctx, id, domain.APIKeyStatusPaused)
}

// Resume flips status from "paused" back to "active". Only the paused ->
// active transition mutates the row. If the current status is "revoked"
// the call returns ErrAPIKeyRevoked (revoked is terminal and cannot be
// resumed); if it is already active the call is a no-op that returns nil.
func (s *APIKeyStore) Resume(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		domain.APIKeyStatusActive, fmtTime(time.Now().UTC()), id, domain.APIKeyStatusPaused)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}
	return s.explainInactiveTransition(ctx, id, domain.APIKeyStatusActive)
}

// ResetMonthlyUsage zeros the monthly_used counter without touching the
// quota. Called by the month-boundary ticker (not wired yet) or by an
// operator via the admin API on credit-refund / complaint flows.
func (s *APIKeyStore) ResetMonthlyUsage(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE api_keys
		SET monthly_used = 0, updated_at = ?
		WHERE id = ?`,
		fmtTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAPIKeyNotFound
	}
	return nil
}

// explainInactiveTransition disambiguates the "0 rows affected" outcome
// from a Pause / Resume conditional UPDATE. It refetches the row and
// returns:
//   - ErrAPIKeyNotFound  when the id is unknown
//   - ErrAPIKeyRevoked   when the row exists but is revoked (terminal)
//   - nil                when the row is already in the desired target
//     status (idempotent no-op)
func (s *APIKeyStore) explainInactiveTransition(ctx context.Context, id, target string) error {
	k, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if k.Status == domain.APIKeyStatusRevoked {
		return domain.ErrAPIKeyRevoked
	}
	if k.Status == target {
		return nil
	}
	// Any other terminal / unexpected status is surfaced verbatim so an
	// operator sees the actual value rather than a misleading generic
	// error.
	return fmt.Errorf("api key %s: cannot transition status %q -> %q", id, k.Status, target)
}

// List returns every api_keys row, newest first.
func (s *APIKeyStore) List(ctx context.Context) ([]domain.APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// ListByCPAPartner returns every api_keys row scoped to the given CPA
// partner, newest first. An empty partnerID returns an empty slice so a
// misconfigured caller cannot dump every standalone (non-CPA) row. The
// caller is expected to filter by status if a specific state is required
// so this method stays symmetric with List.
func (s *APIKeyStore) ListByCPAPartner(ctx context.Context, partnerID string) ([]domain.APIKey, error) {
	if partnerID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys
		 WHERE cpa_partner_id = ?
		 ORDER BY created_at DESC`, partnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func scanAPIKey(sc scanner) (*domain.APIKey, error) {
	var (
		k            domain.APIKey
		createdBy    sql.NullString
		cpaPartnerID sql.NullString
		groupID      sql.NullString
		lastUsedAt   sql.NullString
		createdAt    string
		markupPct    float64
	)
	if err := sc.Scan(
		&k.ID, &k.KeyHash, &k.Name, &createdBy, &cpaPartnerID, &groupID, &k.Status,
		&k.MonthlyQuota, &k.MonthlyUsed, &markupPct,
		&createdAt, &lastUsedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrAPIKeyNotFound
		}
		return nil, err
	}
	k.CreatedBy = createdBy.String
	k.CPAPartnerID = cpaPartnerID.String
	k.GroupID = groupID.String
	k.MarkupPct = markupPct
	k.CreatedAt = parseTime(createdAt)
	k.LastUsedAt = parseTime(lastUsedAt.String)
	return &k, nil
}

// defaultStatus returns the given status, or "active" when it's empty.
func defaultStatus(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

// defaultMarkup returns the given markup, or 1.0 when it's zero.
func defaultMarkup(m float64) float64 {
	if m == 0 {
		return 1.0
	}
	return m
}

// defaultTime returns the given time, or now.
func defaultTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t
}
