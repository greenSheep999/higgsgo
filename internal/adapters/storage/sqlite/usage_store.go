package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// UsageEventStore implements ports.UsageEventStore backed by SQLite.
//
// Rows land in the usage_events table (created by migration 001_init.sql).
// Insert is called by the metering recorder on every terminal job. Query
// serves detail listings for the admin/report endpoints, and Aggregate
// powers dashboard rollups without touching the pre-aggregated
// usage_daily_agg table (that rollup is written by a separate ticker).
type UsageEventStore struct {
	db *DB
}

// NewUsageEventStore returns a fresh UsageEventStore rooted at the given DB.
func NewUsageEventStore(db *DB) *UsageEventStore { return &UsageEventStore{db: db} }

// allowedGroupByCols is the whitelist of columns callers may pass in
// UsageAggQuery.GroupBy. Anything outside this set is dropped silently so a
// bad caller cannot inject SQL through the aggregation path.
//
// `billing_hour` is a synthetic bucket derived from `ts` (see
// groupByExpression) rather than a real column — added so the webui can
// render an intraday trend without pulling raw events.
var allowedGroupByCols = map[string]struct{}{
	"api_key_id":     {},
	"cpa_partner_id": {},
	"account_id":     {},
	"group_id":       {},
	"model_alias":    {},
	"billing_hour":   {},
	"billing_day":    {},
	"billing_month":  {},
}

// groupByExpression returns the SQL expression the SELECT/GROUP BY uses for
// a whitelisted group-by column. Real columns are echoed as-is; synthetic
// buckets like billing_hour compile to a strftime() call on ts.
func groupByExpression(col string) string {
	switch col {
	case "billing_hour":
		// ISO-style "YYYY-MM-DDTHH:00:00Z" so the client can sort as string
		// and parse as RFC3339 without extra munging.
		return "strftime('%Y-%m-%dT%H:00:00Z', ts)"
	default:
		return col
	}
}

// Insert writes a single UsageEvent row.
func (s *UsageEventStore) Insert(ctx context.Context, e *domain.UsageEvent) error {
	if e == nil {
		return errors.New("insert usage event: nil event")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_events (
			id, ts,
			api_key_id, cpa_partner_id, cpa_user_id,
			group_id, account_id,
			model_alias, jst, media_type,
			upstream_cost, actual_credits_h, charged_credits_h, markup_pct,
			status, latency_ms, poll_count, error_type,
			higgsgo_job_id, upstream_job_id, result_url,
			billing_month, billing_day
		) VALUES (
			?, ?,
			?, ?, ?,
			?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?
		)`,
		e.ID, fmtTime(e.TS),
		nullStr(e.APIKeyID), nullStr(e.CPAPartnerID), nullStr(e.CPAUserID),
		e.GroupID, e.AccountID,
		e.ModelAlias, e.JST, e.MediaType,
		nullInt(e.UpstreamCost), e.ActualCreditsHundredths, e.ChargedCreditsHundredths, e.MarkupPct,
		string(e.Status), nullInt(e.LatencyMS), nullInt(int64(e.PollCount)), nullStr(string(e.ErrorType)),
		e.HiggsgoJobID, nullStr(e.UpstreamJobID), nullStr(e.ResultURL),
		e.BillingMonth, e.BillingDay,
	)
	if err != nil {
		return fmt.Errorf("insert usage event %s: %w", e.ID, err)
	}
	return nil
}

// Query returns UsageEvent rows matching the filter, newest first.
func (s *UsageEventStore) Query(ctx context.Context, q ports.UsageQuery) ([]domain.UsageEvent, error) {
	clauses, args := buildUsageFilterClauses(q)

	sqlStr := `SELECT id, ts,
	           api_key_id, cpa_partner_id, cpa_user_id,
	           group_id, account_id,
	           model_alias, jst, media_type,
	           upstream_cost, actual_credits_h, charged_credits_h, markup_pct,
	           status, latency_ms, poll_count, error_type,
	           higgsgo_job_id, upstream_job_id, result_url,
	           billing_month, billing_day
	           FROM usage_events`
	if len(clauses) > 0 {
		sqlStr += " WHERE " + strings.Join(clauses, " AND ")
	}
	sqlStr += " ORDER BY ts DESC"
	if q.Limit > 0 {
		sqlStr += fmt.Sprintf(" LIMIT %d", q.Limit)
		if q.Offset > 0 {
			sqlStr += fmt.Sprintf(" OFFSET %d", q.Offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("query usage events: %w", err)
	}
	defer rows.Close()

	var out []domain.UsageEvent
	for rows.Next() {
		e, err := scanUsageEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// Aggregate runs a GROUP BY over usage_events and returns one UsageAggRow per
// distinct combination of the requested dimensions.
//
// Metrics returned per row:
//   - request_count: total events
//   - completed_count / failed_count / refunded_count: split by status
//   - total_credits_h: SUM(actual_credits_h)
//   - charged_credits_h: SUM(charged_credits_h)
//   - avg_latency_ms: AVG(latency_ms) over rows with a non-null latency
func (s *UsageEventStore) Aggregate(ctx context.Context, q ports.UsageAggQuery) ([]ports.UsageAggRow, error) {
	// Sanitize group-by columns against the whitelist.
	var groupCols []string
	for _, col := range q.GroupBy {
		if _, ok := allowedGroupByCols[col]; ok {
			groupCols = append(groupCols, col)
		}
	}

	// Assemble filter using both the top-level Since/Until on UsageAggQuery
	// and any secondary filters passed via q.Filters.
	filters := q.Filters
	if !q.Since.IsZero() {
		filters.Since = q.Since
	}
	if !q.Until.IsZero() {
		filters.Until = q.Until
	}
	clauses, args := buildUsageFilterClauses(filters)

	// Some group-by columns are synthetic (derived from ts, etc.). We select
	// them via their expression aliased back to the column name so the scan
	// path is uniform, and GROUP BY / ORDER BY use the expression directly
	// because sqlite doesn't honour output aliases in GROUP BY reliably.
	var selectCols []string
	var groupExprs []string
	for _, col := range groupCols {
		expr := groupByExpression(col)
		if expr == col {
			selectCols = append(selectCols, col)
		} else {
			selectCols = append(selectCols, expr+" AS "+col)
		}
		groupExprs = append(groupExprs, expr)
	}
	selectCols = append(selectCols,
		// COUNT never NULLs, but the three status SUMs collapse to NULL
		// on an empty result set (no GROUP BY + no matching rows). Wrap
		// each in COALESCE so scan targets stay int64 without a nullable
		// intermediate — the caller wants 0, not "unknown".
		"COUNT(*) AS request_count",
		"COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0) AS completed_count",
		"COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0) AS failed_count",
		"COALESCE(SUM(CASE WHEN status = 'refunded' THEN 1 ELSE 0 END), 0) AS refunded_count",
		"COALESCE(SUM(actual_credits_h), 0) AS total_credits_h",
		"COALESCE(SUM(charged_credits_h), 0) AS charged_credits_h",
		"COALESCE(CAST(AVG(latency_ms) AS INTEGER), 0) AS avg_latency_ms",
	)

	sqlStr := "SELECT " + strings.Join(selectCols, ", ") + " FROM usage_events"
	if len(clauses) > 0 {
		sqlStr += " WHERE " + strings.Join(clauses, " AND ")
	}
	if len(groupExprs) > 0 {
		sqlStr += " GROUP BY " + strings.Join(groupExprs, ", ")
		sqlStr += " ORDER BY " + strings.Join(groupExprs, ", ")
	}

	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate usage events: %w", err)
	}
	defer rows.Close()

	var out []ports.UsageAggRow
	for rows.Next() {
		// Prepare dynamic scan destinations: one *sql.NullString per group
		// column, then fixed metric columns.
		keyPtrs := make([]sql.NullString, len(groupCols))
		scanArgs := make([]any, 0, len(groupCols)+7)
		for i := range keyPtrs {
			scanArgs = append(scanArgs, &keyPtrs[i])
		}
		var row ports.UsageAggRow
		scanArgs = append(scanArgs,
			&row.RequestCount,
			&row.CompletedCount,
			&row.FailedCount,
			&row.RefundedCount,
			&row.TotalCreditsHundredths,
			&row.ChargedCreditsHundredths,
			&row.AvgLatencyMS,
		)
		if err := rows.Scan(scanArgs...); err != nil {
			return nil, fmt.Errorf("scan usage agg row: %w", err)
		}
		if len(groupCols) > 0 {
			row.Keys = make(map[string]string, len(groupCols))
			for i, col := range groupCols {
				row.Keys[col] = keyPtrs[i].String
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// buildUsageFilterClauses converts a UsageQuery into WHERE fragments and args.
func buildUsageFilterClauses(q ports.UsageQuery) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	if !q.Since.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, fmtTime(q.Since))
	}
	if !q.Until.IsZero() {
		clauses = append(clauses, "ts < ?")
		args = append(args, fmtTime(q.Until))
	}
	if q.APIKeyID != "" {
		clauses = append(clauses, "api_key_id = ?")
		args = append(args, q.APIKeyID)
	}
	if q.CPAPartnerID != "" {
		clauses = append(clauses, "cpa_partner_id = ?")
		args = append(args, q.CPAPartnerID)
	}
	if q.AccountID != "" {
		clauses = append(clauses, "account_id = ?")
		args = append(args, q.AccountID)
	}
	if q.GroupID != "" {
		clauses = append(clauses, "group_id = ?")
		args = append(args, q.GroupID)
	}
	if q.ModelAlias != "" {
		clauses = append(clauses, "model_alias = ?")
		args = append(args, q.ModelAlias)
	}
	if q.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, q.Status)
	}
	return clauses, args
}

// scanUsageEvent reads one usage_events row into a domain.UsageEvent.
func scanUsageEvent(sc scanner) (*domain.UsageEvent, error) {
	var (
		e             domain.UsageEvent
		ts            string
		apiKeyID      sql.NullString
		cpaPartnerID  sql.NullString
		cpaUserID     sql.NullString
		upstreamCost  sql.NullInt64
		latencyMS     sql.NullInt64
		pollCount     sql.NullInt64
		errorType     sql.NullString
		upstreamJobID sql.NullString
		resultURL     sql.NullString
		statusStr     string
	)
	if err := sc.Scan(
		&e.ID, &ts,
		&apiKeyID, &cpaPartnerID, &cpaUserID,
		&e.GroupID, &e.AccountID,
		&e.ModelAlias, &e.JST, &e.MediaType,
		&upstreamCost, &e.ActualCreditsHundredths, &e.ChargedCreditsHundredths, &e.MarkupPct,
		&statusStr, &latencyMS, &pollCount, &errorType,
		&e.HiggsgoJobID, &upstreamJobID, &resultURL,
		&e.BillingMonth, &e.BillingDay,
	); err != nil {
		return nil, err
	}
	e.TS = parseTime(ts)
	e.APIKeyID = apiKeyID.String
	e.CPAPartnerID = cpaPartnerID.String
	e.CPAUserID = cpaUserID.String
	e.UpstreamCost = upstreamCost.Int64
	e.LatencyMS = latencyMS.Int64
	e.PollCount = int(pollCount.Int64)
	e.ErrorType = domain.ErrorType(errorType.String)
	e.UpstreamJobID = upstreamJobID.String
	e.ResultURL = resultURL.String
	e.Status = domain.JobStatus(statusStr)
	return &e, nil
}
