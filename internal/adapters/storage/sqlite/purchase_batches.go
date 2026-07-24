package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// The PurchaseBatch table methods live on the existing PricingStore so
// the pricing sub-domain stays behind a single receiver. Migration 025
// creates the table; this file's methods assume it exists.

// ListPurchaseBatches returns every row in purchase_batches ordered
// newest purchase first. Callers (the CRUD UI, the weighted-average
// calculator) filter down in application code rather than pushing a
// WHERE clause here — the table is expected to stay under a few
// thousand rows even long-term, and one COALESCE-friendly query is
// simpler to keep in sync with the domain type.
func (s *PricingStore) ListPurchaseBatches(ctx context.Context) ([]domain.PurchaseBatch, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, purchased_at, source_channel, source_seller, plan_type,
		       accounts_count, credits_per_account_hundredths, total_paid_micros,
		       paid_currency, paid_amount_original_micros, exchange_rate_used,
		       pricing_class, promotion_type, active, linked_account_email, rationale,
		       created_at, updated_at
		FROM purchase_batches
		ORDER BY purchased_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PurchaseBatch
	for rows.Next() {
		batch, err := scanPurchaseBatchRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *batch)
	}
	return out, rows.Err()
}

// GetPurchaseBatch is the by-id read. Returns (nil, nil) when the row
// doesn't exist so the admin CRUD can 404 without string-matching on
// sql.ErrNoRows.
func (s *PricingStore) GetPurchaseBatch(ctx context.Context, id string) (*domain.PurchaseBatch, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, purchased_at, source_channel, source_seller, plan_type,
		       accounts_count, credits_per_account_hundredths, total_paid_micros,
		       paid_currency, paid_amount_original_micros, exchange_rate_used,
		       pricing_class, promotion_type, active, linked_account_email, rationale,
		       created_at, updated_at
		FROM purchase_batches WHERE id = ?`, id)
	batch, err := scanPurchaseBatchRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// UpsertPurchaseBatch inserts a new row or updates an existing one.
// Insert semantics: caller supplies all fields including CreatedAt.
// Update semantics: CreatedAt is IGNORED (preserved from the existing
// row) so the audit trail stays honest; UpdatedAt is refreshed here.
func (s *PricingStore) UpsertPurchaseBatch(ctx context.Context, batch *domain.PurchaseBatch) error {
	if batch == nil || batch.ID == "" {
		return errors.New("purchase batch id is required")
	}
	now := time.Now().UTC()
	batch.UpdatedAt = now
	if batch.CreatedAt.IsZero() {
		batch.CreatedAt = now
	}
	activeInt := 0
	if batch.Active {
		activeInt = 1
	}
	if batch.PromotionType == "" {
		batch.PromotionType = "none"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO purchase_batches (
			id, purchased_at, source_channel, source_seller, plan_type,
			accounts_count, credits_per_account_hundredths, total_paid_micros,
			paid_currency, paid_amount_original_micros, exchange_rate_used,
			pricing_class, promotion_type, active, linked_account_email, rationale,
			created_at, updated_at
		) VALUES (?,?,?,?,?, ?,?,?, ?,?,?, ?,?,?,?,?, ?,?)
		ON CONFLICT(id) DO UPDATE SET
			purchased_at = excluded.purchased_at,
			source_channel = excluded.source_channel,
			source_seller = excluded.source_seller,
			plan_type = excluded.plan_type,
			accounts_count = excluded.accounts_count,
			credits_per_account_hundredths = excluded.credits_per_account_hundredths,
			total_paid_micros = excluded.total_paid_micros,
			paid_currency = excluded.paid_currency,
			paid_amount_original_micros = excluded.paid_amount_original_micros,
			exchange_rate_used = excluded.exchange_rate_used,
			pricing_class = excluded.pricing_class,
			promotion_type = excluded.promotion_type,
			active = excluded.active,
			linked_account_email = excluded.linked_account_email,
			rationale = excluded.rationale,
			updated_at = excluded.updated_at
		`,
		batch.ID, fmtTime(batch.PurchasedAt), batch.SourceChannel, batch.SourceSeller, batch.PlanType,
		batch.AccountsCount, batch.CreditsPerAccountHundredths, batch.TotalPaidMicros,
		batch.PaidCurrency, batch.PaidAmountOriginalMicros, batch.ExchangeRateUsed,
		batch.PricingClass, batch.PromotionType, activeInt, batch.LinkedAccountEmail, batch.Rationale,
		fmtTime(batch.CreatedAt), fmtTime(batch.UpdatedAt),
	)
	return err
}

// DeletePurchaseBatch is idempotent: missing rows return nil, not an
// error. Callers who need "did anything happen?" can Get first.
func (s *PricingStore) DeletePurchaseBatch(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM purchase_batches WHERE id = ?`, id)
	return err
}

// scanPurchaseBatchRow is shared between Query (multi-row) and QueryRow
// (single) callers. The `interface { Scan(...) }` signature covers both.
// Keeping the column order pinned in one place prevents the two callers
// from drifting.
func scanPurchaseBatchRow(row interface{ Scan(...any) error }) (*domain.PurchaseBatch, error) {
	var b domain.PurchaseBatch
	var purchasedAt, createdAt, updatedAt string
	var activeInt int
	if err := row.Scan(
		&b.ID, &purchasedAt, &b.SourceChannel, &b.SourceSeller, &b.PlanType,
		&b.AccountsCount, &b.CreditsPerAccountHundredths, &b.TotalPaidMicros,
		&b.PaidCurrency, &b.PaidAmountOriginalMicros, &b.ExchangeRateUsed,
		&b.PricingClass, &b.PromotionType, &activeInt, &b.LinkedAccountEmail, &b.Rationale,
		&createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	b.PurchasedAt = parseTime(purchasedAt)
	b.CreatedAt = parseTime(createdAt)
	b.UpdatedAt = parseTime(updatedAt)
	b.Active = activeInt != 0
	return &b, nil
}
