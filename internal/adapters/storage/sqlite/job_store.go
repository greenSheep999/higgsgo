package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// defaultJobListLimit is applied when JobFilter.Limit == 0.
const defaultJobListLimit = 100

// maxJobListLimit caps JobFilter.Limit so a single request cannot page the
// full jobs table.
const maxJobListLimit = 500

// JobStore implements ports.JobStore backed by SQLite.
type JobStore struct {
	db *DB
}

// NewJobStore returns a JobStore rooted at the given DB.
func NewJobStore(db *DB) *JobStore { return &JobStore{db: db} }

// Create inserts a new job record. callback_url and pre_balance_h are
// persisted verbatim (both default to empty/zero when omitted); the async
// pollworker consumes them at terminal transition.
func (s *JobStore) Create(ctx context.Context, j *domain.Job) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (
			id, api_key_id, cpa_partner_id, group_id, account_id,
			model_alias, jst, endpoint, request_body_json, request_ts,
			upstream_job_id, upstream_cost, result_url,
			status, error_type, error_detail,
			latency_ms, poll_count,
			actual_credits_h, charged_credits_h, refunded,
			callback_url, pre_balance_h
		) VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?, ?,?,?, ?,?, ?,?,?, ?,?)`,
		j.ID, nullStr(j.APIKeyID), nullStr(j.CPAPartnerID), nullStr(j.GroupID), j.AccountID,
		j.ModelAlias, j.JST, j.Endpoint, j.RequestBodyJSON, fmtTime(j.RequestTS),
		nullStr(j.UpstreamJobID), nullInt(j.UpstreamCost), nullStr(j.ResultURL),
		string(j.Status), nullStr(string(j.ErrorType)), nullStr(j.ErrorDetail),
		j.LatencyMS, j.PollCount,
		nullInt(j.ActualCreditsHundredths), nullInt(j.ChargedCreditsHundredths), boolToInt(j.Refunded),
		j.CallbackURL, j.PreBalanceH,
	)
	if err != nil {
		return fmt.Errorf("insert job %s: %w", j.ID, err)
	}
	return nil
}

// TryMarkTerminal atomically moves a job into `to` iff its current status
// is in `from`. Returns won=true when this call performed the write; a
// won=false result means another observer (sync path vs pollworker) already
// terminated the job and this caller MUST skip its side effects — metering,
// webhook fire, and in-flight release — because the winner already ran
// them. See F1 in the ROADMAP.
//
// meta fields are applied only when this call wins: the columns follow the
// same "append when non-zero" shape as UpdateStatus so the winner's outcome
// (result URL, latency, poll count, actual/charged credits, refund flag)
// lands on the row, while a loser's staler snapshot cannot overwrite it.
//
// This is intentionally NOT a general-purpose UpdateStatus replacement.
// Non-terminal progress writes (queued → in_progress, per-tick poll_count
// bumps) still go through UpdateStatus; only the terminal transition needs
// the CAS guard.
//
// A won=false outcome is NOT an error — it is a race-lost signal. Real
// errors (bad SQL, closed DB) still bubble up as err != nil.
func (s *JobStore) TryMarkTerminal(
	ctx context.Context,
	id string,
	from []domain.JobStatus,
	to domain.JobStatus,
	meta ports.JobMeta,
) (bool, error) {
	if len(from) == 0 {
		// A caller with no from statuses has misconfigured the guard;
		// refuse to touch the row rather than degrade to an unguarded
		// UPDATE.
		return false, fmt.Errorf("try mark terminal %s: from statuses required", id)
	}
	if !isTerminal(to) {
		// Defence in depth: TryMarkTerminal is for terminal transitions
		// only. A caller trying to CAS into a non-terminal state (e.g.
		// in_progress) should keep using UpdateStatus.
		return false, fmt.Errorf("try mark terminal %s: %q is not a terminal status", id, to)
	}

	now := fmtTime(time.Now())
	q := `UPDATE jobs SET status = ?, last_poll_at = ?, finished_at = ?`
	args := []any{string(to), now, now}
	if meta.UpstreamJobID != "" {
		q += `, upstream_job_id = ?`
		args = append(args, meta.UpstreamJobID)
	}
	if meta.ResultURL != "" {
		q += `, result_url = ?`
		args = append(args, meta.ResultURL)
	}
	if meta.ErrorType != "" {
		q += `, error_type = ?`
		args = append(args, string(meta.ErrorType))
	}
	if meta.ErrorDetail != "" {
		q += `, error_detail = ?`
		args = append(args, meta.ErrorDetail)
	}
	if meta.LatencyMS > 0 {
		q += `, latency_ms = ?`
		args = append(args, meta.LatencyMS)
	}
	if meta.PollCount > 0 {
		q += `, poll_count = ?`
		args = append(args, meta.PollCount)
	}
	if meta.ActualCreditsHundredths != 0 {
		q += `, actual_credits_h = ?`
		args = append(args, meta.ActualCreditsHundredths)
	}
	if meta.ChargedCreditsHundredths != 0 {
		q += `, charged_credits_h = ?`
		args = append(args, meta.ChargedCreditsHundredths)
	}
	if meta.Refunded {
		q += `, refunded = 1`
	}

	// Expand from-status placeholders inline to preserve parameterisation
	// (no string interpolation of caller-supplied statuses).
	placeholders := make([]string, len(from))
	for i, st := range from {
		placeholders[i] = "?"
		_ = st // used below
	}
	q += ` WHERE id = ? AND status IN (` + strings.Join(placeholders, ",") + `)`
	args = append(args, id)
	for _, st := range from {
		args = append(args, string(st))
	}

	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("try mark terminal %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("try mark terminal %s rows affected: %w", id, err)
	}
	// n == 0 has two possible causes: (a) the row exists but its status
	// is already outside `from` — the race-loss case — or (b) the row
	// does not exist at all. Both are treated as "lost the race, do not
	// run side effects". We deliberately do NOT return ErrJobNotFound
	// here: a caller that has been polling a live job just witnessed
	// another observer terminate it, which is exactly the F1 race path.
	return n == 1, nil
}

// UpdateStatus records a status transition (queued → in_progress → completed/failed/etc).
// meta.LatencyMS / meta.PollCount / meta.ResultURL are applied when non-zero.
//
// Terminal transitions should use TryMarkTerminal instead so metering /
// webhook / in-flight release can be gated on a single winner. UpdateStatus
// remains the right call for non-terminal progress writes and for the
// metering back-fill in core/metering (where UpdateStatus's ErrJobNotFound
// return is load-bearing for "cf_*" synthetic-id rows without a jobs row).
func (s *JobStore) UpdateStatus(ctx context.Context, id string, status domain.JobStatus, meta ports.JobMeta) error {
	// Build UPDATE dynamically so we don't clobber fields set by prior calls.
	q := `UPDATE jobs SET status = ?, last_poll_at = ?`
	args := []any{string(status), fmtTime(time.Now())}
	if isTerminal(status) {
		q += `, finished_at = ?`
		args = append(args, fmtTime(time.Now()))
	}
	if meta.UpstreamJobID != "" {
		q += `, upstream_job_id = ?`
		args = append(args, meta.UpstreamJobID)
	}
	if meta.ResultURL != "" {
		q += `, result_url = ?`
		args = append(args, meta.ResultURL)
	}
	if meta.ErrorType != "" {
		q += `, error_type = ?`
		args = append(args, string(meta.ErrorType))
	}
	if meta.ErrorDetail != "" {
		q += `, error_detail = ?`
		args = append(args, meta.ErrorDetail)
	}
	if meta.LatencyMS > 0 {
		q += `, latency_ms = ?`
		args = append(args, meta.LatencyMS)
	}
	if meta.PollCount > 0 {
		q += `, poll_count = ?`
		args = append(args, meta.PollCount)
	}
	if meta.ActualCreditsHundredths != 0 {
		q += `, actual_credits_h = ?`
		args = append(args, meta.ActualCreditsHundredths)
	}
	if meta.ChargedCreditsHundredths != 0 {
		q += `, charged_credits_h = ?`
		args = append(args, meta.ChargedCreditsHundredths)
	}
	if meta.Refunded {
		q += `, refunded = 1`
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update job %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrJobNotFound
	}
	return nil
}

// Get fetches a single job by id.
func (s *JobStore) Get(ctx context.Context, id string) (*domain.Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, api_key_id, cpa_partner_id, group_id, account_id,
		       model_alias, jst, endpoint, request_body_json, request_ts,
		       upstream_job_id, upstream_cost, result_url,
		       status, error_type, error_detail, finished_at,
		       latency_ms, poll_count,
		       actual_credits_h, charged_credits_h, refunded,
		       callback_url, pre_balance_h
		FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

// ListByAPIKey returns jobs authored by apiKeyID, newest first.
//
// filter narrows the result by status and request_ts range; Limit defaults
// to defaultJobListLimit (100) and is capped at maxJobListLimit (500). An
// empty apiKeyID short-circuits to zero rows so a caller with a missing
// context value cannot accidentally page the whole table.
func (s *JobStore) ListByAPIKey(ctx context.Context, apiKeyID string, filter ports.JobFilter) ([]domain.Job, error) {
	if apiKeyID == "" {
		return nil, nil
	}
	clauses := []string{"api_key_id = ?"}
	args := []any{apiKeyID}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}
	// request_ts is persisted as an RFC3339 string (see fmtTime); RFC3339
	// is lexicographically sortable so string comparison matches temporal
	// order. Same trick used by UsageEventStore.Query for its ts column.
	if !filter.Since.IsZero() {
		clauses = append(clauses, "request_ts >= ?")
		args = append(args, fmtTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		clauses = append(clauses, "request_ts < ?")
		args = append(args, fmtTime(filter.Until))
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultJobListLimit
	}
	if limit > maxJobListLimit {
		limit = maxJobListLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	sqlStr := `
		SELECT id, api_key_id, cpa_partner_id, group_id, account_id,
		       model_alias, jst, endpoint, request_body_json, request_ts,
		       upstream_job_id, upstream_cost, result_url,
		       status, error_type, error_detail, finished_at,
		       latency_ms, poll_count,
		       actual_credits_h, charged_credits_h, refunded,
		       callback_url, pre_balance_h
		FROM jobs
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY request_ts DESC
		LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs by api key %s: %w", apiKeyID, err)
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ListAll returns jobs across the entire table, newest first.
//
// Unlike ListByAPIKey there is no mandatory scope: every JobFilter field
// is optional. When all filters are empty the query returns the newest
// Limit rows (defaultJobListLimit, capped at maxJobListLimit). Intended
// for the operator-facing /admin/jobs surface; the public /v1/jobs path
// keeps its api_key_id scoping via ListByAPIKey.
func (s *JobStore) ListAll(ctx context.Context, filter ports.JobFilter) ([]domain.Job, error) {
	var clauses []string
	var args []any
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.AccountID != "" {
		clauses = append(clauses, "account_id = ?")
		args = append(args, filter.AccountID)
	}
	if filter.APIKeyID != "" {
		clauses = append(clauses, "api_key_id = ?")
		args = append(args, filter.APIKeyID)
	}
	if filter.GroupID != "" {
		clauses = append(clauses, "group_id = ?")
		args = append(args, filter.GroupID)
	}
	if filter.ModelAlias != "" {
		clauses = append(clauses, "model_alias = ?")
		args = append(args, filter.ModelAlias)
	}
	// request_ts is persisted as an RFC3339 string (see fmtTime); RFC3339
	// is lexicographically sortable so string comparison matches temporal
	// order. Same trick used by ListByAPIKey above.
	if !filter.Since.IsZero() {
		clauses = append(clauses, "request_ts >= ?")
		args = append(args, fmtTime(filter.Since))
	}
	if !filter.Until.IsZero() {
		clauses = append(clauses, "request_ts < ?")
		args = append(args, fmtTime(filter.Until))
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultJobListLimit
	}
	if limit > maxJobListLimit {
		limit = maxJobListLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	sqlStr := `
		SELECT id, api_key_id, cpa_partner_id, group_id, account_id,
		       model_alias, jst, endpoint, request_body_json, request_ts,
		       upstream_job_id, upstream_cost, result_url,
		       status, error_type, error_detail, finished_at,
		       latency_ms, poll_count,
		       actual_credits_h, charged_credits_h, refunded,
		       callback_url, pre_balance_h
		FROM jobs`
	if len(clauses) > 0 {
		sqlStr += "\n\t\tWHERE " + strings.Join(clauses, " AND ")
	}
	sqlStr += "\n\t\tORDER BY request_ts DESC\n\t\tLIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("list all jobs: %w", err)
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Purge deletes terminal-status jobs whose finished_at is strictly older
// than olderThan. Callers pass the terminal statuses they want to sweep
// (completed / failed / refunded / timeout); passing an empty slice is a
// no-op so a mis-configured caller cannot wipe every finished job by
// omitting the filter.
//
// Runs inside a transaction and returns the number of rows removed. The
// usage_events table is intentionally untouched: the accounting detail
// rows survive so operators can still query historical billing after the
// jobs row is gone.
func (s *JobStore) Purge(ctx context.Context, olderThan time.Time, statuses []domain.JobStatus) (int, error) {
	if len(statuses) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(statuses))
	args := make([]any, 0, len(statuses)+1)
	for i, st := range statuses {
		placeholders[i] = "?"
		args = append(args, string(st))
	}
	args = append(args, fmtTime(olderThan))
	q := `DELETE FROM jobs
		WHERE status IN (` + strings.Join(placeholders, ",") + `)
		  AND finished_at IS NOT NULL
		  AND finished_at < ?`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("purge begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("purge exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge rows affected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("purge commit: %w", err)
	}
	return int(n), nil
}

// ListPending returns all jobs whose status is queued or in_progress.
// Ordered by request_ts ascending so oldest jobs poll first — this is what
// the background worker consumes.
func (s *JobStore) ListPending(ctx context.Context) ([]domain.Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, api_key_id, cpa_partner_id, group_id, account_id,
		       model_alias, jst, endpoint, request_body_json, request_ts,
		       upstream_job_id, upstream_cost, result_url,
		       status, error_type, error_detail, finished_at,
		       latency_ms, poll_count,
		       actual_credits_h, charged_credits_h, refunded,
		       callback_url, pre_balance_h
		FROM jobs
		WHERE status IN ('pending','queued','in_progress')
		ORDER BY request_ts ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// scanJob reads one jobs row into a domain.Job. Column order must match
// the SELECT lists in Get / ListPending exactly.
func scanJob(sc scanner) (*domain.Job, error) {
	var (
		j              domain.Job
		apiKeyID       sql.NullString
		cpaPartnerID   sql.NullString
		groupID        sql.NullString
		upstreamJobID  sql.NullString
		upstreamCost   sql.NullInt64
		resultURL      sql.NullString
		errorType      sql.NullString
		errorDetail    sql.NullString
		finishedAt     sql.NullString
		// latency_ms is NULL for pending / running rows (only stamped
		// at the terminal transition). Scan via NullInt64 so
		// pool_collector + pollworker's ListPending don't crash on
		// live queue rows. Same for poll_count which historically
		// defaulted to 0 but older migrations may have left NULL.
		latencyMS      sql.NullInt64
		pollCount      sql.NullInt64
		actualCredits  sql.NullInt64
		chargedCredits sql.NullInt64
		refunded       int
		statusStr      string
		requestTS      string
		callbackURL    string
		preBalanceH    int64
	)
	if err := sc.Scan(
		&j.ID, &apiKeyID, &cpaPartnerID, &groupID, &j.AccountID,
		&j.ModelAlias, &j.JST, &j.Endpoint, &j.RequestBodyJSON, &requestTS,
		&upstreamJobID, &upstreamCost, &resultURL,
		&statusStr, &errorType, &errorDetail, &finishedAt,
		&latencyMS, &pollCount,
		&actualCredits, &chargedCredits, &refunded,
		&callbackURL, &preBalanceH,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrJobNotFound
		}
		return nil, err
	}
	j.LatencyMS = latencyMS.Int64
	j.PollCount = int(pollCount.Int64)
	j.APIKeyID = apiKeyID.String
	j.CPAPartnerID = cpaPartnerID.String
	j.GroupID = groupID.String
	j.UpstreamJobID = upstreamJobID.String
	j.UpstreamCost = upstreamCost.Int64
	j.ResultURL = resultURL.String
	j.Status = domain.JobStatus(statusStr)
	j.ErrorType = domain.ErrorType(errorType.String)
	j.ErrorDetail = errorDetail.String
	j.RequestTS = parseTime(requestTS)
	j.FinishedAt = parseTime(finishedAt.String)
	j.ActualCreditsHundredths = actualCredits.Int64
	j.ChargedCreditsHundredths = chargedCredits.Int64
	j.Refunded = intToBool(refunded)
	j.CallbackURL = callbackURL
	j.PreBalanceH = preBalanceH
	return &j, nil
}

// nullStr wraps a string for OR NULL insertion.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInt wraps an int64 for OR NULL insertion (0 → NULL).
func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// isTerminal duplicates the check in core/upstream so we don't create a
// cross-package dependency for a single string check.
func isTerminal(s domain.JobStatus) bool {
	switch s {
	case domain.JobCompleted, domain.JobFailed, domain.JobRefunded, domain.JobTimeout:
		return true
	}
	return false
}
