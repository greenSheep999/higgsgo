package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// JobStore implements ports.JobStore backed by SQLite.
type JobStore struct {
	db *DB
}

// NewJobStore returns a JobStore rooted at the given DB.
func NewJobStore(db *DB) *JobStore { return &JobStore{db: db} }

// Create inserts a new job record.
func (s *JobStore) Create(ctx context.Context, j *domain.Job) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (
			id, api_key_id, cpa_partner_id, group_id, account_id,
			model_alias, jst, endpoint, request_body_json, request_ts,
			upstream_job_id, upstream_cost, result_url,
			status, error_type, error_detail,
			latency_ms, poll_count,
			actual_credits_h, charged_credits_h, refunded
		) VALUES (?,?,?,?,?, ?,?,?,?,?, ?,?,?, ?,?,?, ?,?, ?,?,?)`,
		j.ID, nullStr(j.APIKeyID), nullStr(j.CPAPartnerID), nullStr(j.GroupID), j.AccountID,
		j.ModelAlias, j.JST, j.Endpoint, j.RequestBodyJSON, fmtTime(j.RequestTS),
		nullStr(j.UpstreamJobID), nullInt(j.UpstreamCost), nullStr(j.ResultURL),
		string(j.Status), nullStr(string(j.ErrorType)), nullStr(j.ErrorDetail),
		j.LatencyMS, j.PollCount,
		nullInt(j.ActualCreditsHundredths), nullInt(j.ChargedCreditsHundredths), boolToInt(j.Refunded),
	)
	if err != nil {
		return fmt.Errorf("insert job %s: %w", j.ID, err)
	}
	return nil
}

// UpdateStatus records a status transition (queued → in_progress → completed/failed/etc).
// meta.LatencyMS / meta.PollCount / meta.ResultURL are applied when non-zero.
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
		       actual_credits_h, charged_credits_h, refunded
		FROM jobs WHERE id = ?`, id)
	return scanJob(row)
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
		       actual_credits_h, charged_credits_h, refunded
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

// scanJob reads one jobs row into a domain.Job.
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
		actualCredits  sql.NullInt64
		chargedCredits sql.NullInt64
		refunded       int
		statusStr      string
		requestTS      string
	)
	if err := sc.Scan(
		&j.ID, &apiKeyID, &cpaPartnerID, &groupID, &j.AccountID,
		&j.ModelAlias, &j.JST, &j.Endpoint, &j.RequestBodyJSON, &requestTS,
		&upstreamJobID, &upstreamCost, &resultURL,
		&statusStr, &errorType, &errorDetail, &finishedAt,
		&j.LatencyMS, &j.PollCount,
		&actualCredits, &chargedCredits, &refunded,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrJobNotFound
		}
		return nil, err
	}
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
