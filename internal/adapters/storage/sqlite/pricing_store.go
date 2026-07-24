package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// PricingStore persists immutable pricing snapshots and normalized rules.
type PricingStore struct {
	db *DB
}

func NewPricingStore(db *DB) *PricingStore { return &PricingStore{db: db} }

func (s *PricingStore) SaveSnapshot(ctx context.Context, snapshot *domain.PricingSnapshot, rules []domain.ModelCostRule) error {
	if snapshot == nil || snapshot.ID == "" || snapshot.Source == "" || snapshot.PayloadJSON == "" {
		return errors.New("save pricing snapshot: id, source, and payload are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pricing snapshot: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO pricing_snapshots (
			id, source, source_url, payload_json, payload_sha256, fetched_at, effective_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		snapshot.ID, snapshot.Source, snapshot.SourceURL, snapshot.PayloadJSON,
		snapshot.PayloadSHA256, fmtTime(snapshot.FetchedAt), fmtTime(snapshot.EffectiveAt),
	)
	if err != nil {
		return fmt.Errorf("insert pricing snapshot: %w", err)
	}

	for i := range rules {
		rule := &rules[i]
		if rule.ID == "" || rule.JST == "" || rule.Unit == "" || rule.CreditsHundredths <= 0 {
			return fmt.Errorf("insert pricing rule %d: id, jst, unit, and positive credits are required", i)
		}
		if rule.SnapshotID == "" {
			rule.SnapshotID = snapshot.ID
		}
		if rule.SnapshotID != snapshot.ID {
			return fmt.Errorf("insert pricing rule %s: snapshot mismatch", rule.ID)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO model_cost_rules (
				id, snapshot_id, jst, model_alias, unit, component,
				credits_hundredths, original_credits_hundredths,
				resolution, duration_seconds, mode, audio, dimensions_json, observed_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rule.ID, rule.SnapshotID, rule.JST, rule.ModelAlias, rule.Unit,
			rule.Component, rule.CreditsHundredths, rule.OriginalCreditsHundredths,
			rule.Resolution, rule.DurationSeconds,
			rule.Mode, rule.Audio, rule.DimensionsJSON, fmtTime(rule.ObservedAt),
		)
		if err != nil {
			return fmt.Errorf("insert pricing rule %s: %w", rule.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pricing snapshot: %w", err)
	}
	return nil
}

func (s *PricingStore) LatestSnapshot(ctx context.Context, source string) (*domain.PricingSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source, source_url, payload_json, payload_sha256, fetched_at, effective_at
		FROM pricing_snapshots
		WHERE source = ?
		ORDER BY fetched_at DESC, id DESC
		LIMIT 1`, source)
	var snapshot domain.PricingSnapshot
	var fetchedAt, effectiveAt string
	if err := row.Scan(&snapshot.ID, &snapshot.Source, &snapshot.SourceURL, &snapshot.PayloadJSON,
		&snapshot.PayloadSHA256, &fetchedAt, &effectiveAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	snapshot.FetchedAt = parseTime(fetchedAt)
	snapshot.EffectiveAt = parseTime(effectiveAt)
	return &snapshot, nil
}

func (s *PricingStore) ListLatestRules(ctx context.Context, source string) ([]domain.ModelCostRule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.snapshot_id, r.jst, r.model_alias, r.unit, r.component,
		       r.credits_hundredths, r.original_credits_hundredths,
		       r.resolution, r.duration_seconds,
		       r.mode, r.audio, r.dimensions_json, r.observed_at
		FROM model_cost_rules r
		JOIN pricing_snapshots s ON s.id = r.snapshot_id
		WHERE s.id = (
			SELECT id FROM pricing_snapshots
			WHERE source = ?
			ORDER BY fetched_at DESC, id DESC
			LIMIT 1
		)
		ORDER BY r.jst, r.resolution, r.duration_seconds, r.mode, r.audio, r.id`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModelCostRule
	for rows.Next() {
		var rule domain.ModelCostRule
		var observedAt string
		if err := rows.Scan(&rule.ID, &rule.SnapshotID, &rule.JST, &rule.ModelAlias,
			&rule.Unit, &rule.Component, &rule.CreditsHundredths,
			&rule.OriginalCreditsHundredths, &rule.Resolution,
			&rule.DurationSeconds, &rule.Mode, &rule.Audio,
			&rule.DimensionsJSON, &observedAt); err != nil {
			return nil, err
		}
		rule.ObservedAt = parseTime(observedAt)
		out = append(out, rule)
	}
	return out, rows.Err()
}

func (s *PricingStore) ListPlanCreditRates(ctx context.Context) ([]domain.PlanCreditRate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, plan_type, plan_name, billing_period, currency,
		       amount_micros, credits, unit_cost_micros, source_url, observed_at
		FROM higgs_plan_rates
		WHERE id IN (
			SELECT id FROM higgs_plan_rates p2
			WHERE p2.plan_type = higgs_plan_rates.plan_type
			  AND p2.billing_period = 'monthly'
			ORDER BY observed_at DESC, id DESC LIMIT 1
		)
		ORDER BY unit_cost_micros, plan_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PlanCreditRate
	for rows.Next() {
		var rate domain.PlanCreditRate
		var observedAt string
		if err := rows.Scan(&rate.ID, &rate.PlanType, &rate.PlanName,
			&rate.BillingPeriod, &rate.Currency, &rate.AmountMicros,
			&rate.Credits, &rate.UnitCostMicros, &rate.SourceURL,
			&observedAt); err != nil {
			return nil, err
		}
		rate.ObservedAt = parseTime(observedAt)
		out = append(out, rate)
	}
	return out, rows.Err()
}

func (s *PricingStore) ListOfficialPrices(ctx context.Context, modelAlias string) ([]domain.OfficialPriceObservation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, model_alias, provider, source_url, currency, unit,
		       price_micros, resolution, duration_seconds, mode, audio,
		       dimensions_json, observed_at, region, estimated
		FROM official_price_observations
		WHERE model_alias = ?
		ORDER BY region, resolution, duration_seconds, mode, audio, observed_at DESC, id DESC`, modelAlias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOfficialPriceRows(rows)
}

// ListAllOfficialPrices returns every row in official_price_observations.
// The order (model_alias first) matters: the /api/pricing/official-api
// handler walks the slice sequentially and groups by model_alias in a
// single pass, so a stable per-alias run of rows keeps the grouping
// loop trivial.
func (s *PricingStore) ListAllOfficialPrices(ctx context.Context) ([]domain.OfficialPriceObservation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, model_alias, provider, source_url, currency, unit,
		       price_micros, resolution, duration_seconds, mode, audio,
		       dimensions_json, observed_at, region, estimated
		FROM official_price_observations
		ORDER BY model_alias, region, resolution, duration_seconds, mode, audio, observed_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOfficialPriceRows(rows)
}

// scanOfficialPriceRows is the shared row-scanning routine for the two
// ListOfficialPrices variants. Kept in one place so the column order
// stays in sync between the per-alias and all-rows queries.
func scanOfficialPriceRows(rows *sql.Rows) ([]domain.OfficialPriceObservation, error) {
	var out []domain.OfficialPriceObservation
	for rows.Next() {
		var price domain.OfficialPriceObservation
		var observedAt string
		var estimated int
		if err := rows.Scan(&price.ID, &price.ModelAlias, &price.Provider,
			&price.SourceURL, &price.Currency, &price.Unit, &price.PriceMicros,
			&price.Resolution, &price.DurationSeconds, &price.Mode, &price.Audio,
			&price.DimensionsJSON, &observedAt, &price.Region, &estimated); err != nil {
			return nil, err
		}
		price.ObservedAt = parseTime(observedAt)
		price.Estimated = estimated == 1
		out = append(out, price)
	}
	return out, rows.Err()
}

// ListLatestPriceDecisions returns the newest row per
// (model_alias, resolution, duration_seconds, mode, audio, unit) tuple.
// The window function is standard SQLite 3.25+ and avoids a two-pass
// GROUP BY that would drop the rationale text.
//
// Ordering is (model_alias, resolution, duration_seconds, mode, audio)
// so the /api/pricing handler can group by model_alias in a single
// linear pass — same pattern as ListAllOfficialPrices.
func (s *PricingStore) ListLatestPriceDecisions(ctx context.Context) ([]domain.ModelPriceDecision, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH ranked AS (
			SELECT id, model_alias, currency, unit, price_micros, resolution,
			       duration_seconds, mode, audio, dimensions_json, rationale, decided_at,
			       ROW_NUMBER() OVER (
			         PARTITION BY model_alias, resolution, duration_seconds, mode, audio, unit
			         ORDER BY decided_at DESC, id DESC
			       ) AS rn
			FROM model_price_decisions
		)
		SELECT id, model_alias, currency, unit, price_micros, resolution,
		       duration_seconds, mode, audio, dimensions_json, rationale, decided_at
		FROM ranked WHERE rn = 1
		ORDER BY model_alias, resolution, duration_seconds, mode, audio`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModelPriceDecision
	for rows.Next() {
		var d domain.ModelPriceDecision
		var decidedAt string
		if err := rows.Scan(&d.ID, &d.ModelAlias, &d.Currency, &d.Unit,
			&d.PriceMicros, &d.Resolution, &d.DurationSeconds, &d.Mode, &d.Audio,
			&d.DimensionsJSON, &d.Rationale, &decidedAt); err != nil {
			return nil, err
		}
		d.DecidedAt = parseTime(decidedAt)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PricingStore) ListPriceDecisions(ctx context.Context, modelAlias string) ([]domain.ModelPriceDecision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, model_alias, currency, unit, price_micros, resolution,
		       duration_seconds, mode, audio, dimensions_json, rationale, decided_at
		FROM model_price_decisions
		WHERE model_alias = ?
		ORDER BY resolution, duration_seconds, mode, audio, decided_at DESC, id DESC`, modelAlias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModelPriceDecision
	for rows.Next() {
		var decision domain.ModelPriceDecision
		var decidedAt string
		if err := rows.Scan(&decision.ID, &decision.ModelAlias, &decision.Currency,
			&decision.Unit, &decision.PriceMicros, &decision.Resolution,
			&decision.DurationSeconds, &decision.Mode, &decision.Audio,
			&decision.DimensionsJSON, &decision.Rationale, &decidedAt); err != nil {
			return nil, err
		}
		decision.DecidedAt = parseTime(decidedAt)
		out = append(out, decision)
	}
	return out, rows.Err()
}

// RecordPriceDecision appends a new operator-approved sell price. Same
// (alias, variant) can be re-recorded; the newest row wins in
// ListPriceDecisions (ORDER BY decided_at DESC). Returns the stored row
// with ID and DecidedAt populated so handlers can echo it back to
// clients.
func (s *PricingStore) RecordPriceDecision(ctx context.Context, decision domain.ModelPriceDecision) (domain.ModelPriceDecision, error) {
	if decision.ModelAlias == "" {
		return domain.ModelPriceDecision{}, errors.New("record price decision: model_alias required")
	}
	if decision.Unit == "" {
		return domain.ModelPriceDecision{}, errors.New("record price decision: unit required")
	}
	if decision.PriceMicros < 0 {
		return domain.ModelPriceDecision{}, errors.New("record price decision: price_micros must be non-negative")
	}
	if decision.Currency == "" {
		decision.Currency = "USD"
	}
	if decision.DimensionsJSON == "" {
		decision.DimensionsJSON = "{}"
	}
	if decision.ID == "" {
		decision.ID = idgen.NewID("prc")
	}
	if decision.DecidedAt.IsZero() {
		decision.DecidedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO model_price_decisions (
			id, model_alias, currency, unit, price_micros, resolution,
			duration_seconds, mode, audio, dimensions_json, rationale, decided_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		decision.ID, decision.ModelAlias, decision.Currency, decision.Unit,
		decision.PriceMicros, decision.Resolution, decision.DurationSeconds,
		decision.Mode, decision.Audio, decision.DimensionsJSON,
		decision.Rationale, fmtTime(decision.DecidedAt),
	)
	if err != nil {
		return domain.ModelPriceDecision{}, fmt.Errorf("insert price decision: %w", err)
	}
	return decision, nil
}
