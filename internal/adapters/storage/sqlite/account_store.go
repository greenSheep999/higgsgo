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

// AccountStore implements ports.AccountStore backed by SQLite.
type AccountStore struct {
	db *DB
}

// NewAccountStore returns an AccountStore rooted at the given DB.
func NewAccountStore(db *DB) *AccountStore { return &AccountStore{db: db} }

// timeFormat is the ISO-8601 format persisted for all timestamps.
const timeFormat = time.RFC3339

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func intToBool(i int) bool { return i != 0 }

// Get returns a single account by id.
func (s *AccountStore) Get(ctx context.Context, id string) (*domain.Account, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, email, password_enc, session_id, cookies_json, user_agent,
		       datadome_client_id, workspace_id, plan_type,
		       has_unlim, has_flex_unlim, is_pro_veo3_available, cohort,
		       subscription_balance, credits_balance, total_plan_credits, plan_ends_at,
		       status, in_flight_jobs, last_balance_at, last_used_at, last_failed_at, fail_streak,
		       bound_proxy_url, registered_at, imported_at
		FROM accounts WHERE id = ?`, id)
	return scanAccount(row)
}

// List returns accounts matching the filter.
func (s *AccountStore) List(ctx context.Context, filter ports.AccountFilter) ([]domain.Account, error) {
	var (
		clauses []string
		args    []any
	)
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.PlanType != "" {
		clauses = append(clauses, "plan_type = ?")
		args = append(args, string(filter.PlanType))
	}
	if filter.MinBalance > 0 {
		clauses = append(clauses, "subscription_balance >= ?")
		args = append(args, filter.MinBalance)
	}
	if filter.HasUnlim != nil {
		clauses = append(clauses, "has_unlim = ?")
		args = append(args, boolToInt(*filter.HasUnlim))
	}

	q := `SELECT id, email, password_enc, session_id, cookies_json, user_agent,
	             datadome_client_id, workspace_id, plan_type,
	             has_unlim, has_flex_unlim, is_pro_veo3_available, cohort,
	             subscription_balance, credits_balance, total_plan_credits, plan_ends_at,
	             status, in_flight_jobs, last_balance_at, last_used_at, last_failed_at, fail_streak,
	             bound_proxy_url, registered_at, imported_at
	      FROM accounts`
	if len(clauses) > 0 {
		q += " WHERE " + strings.Join(clauses, " AND ")
	}
	q += " ORDER BY imported_at DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	var out []domain.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// Upsert inserts a new account or updates an existing one (matched by id).
func (s *AccountStore) Upsert(ctx context.Context, a *domain.Account) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (
			id, email, password_enc, session_id, cookies_json, user_agent,
			datadome_client_id, workspace_id, plan_type,
			has_unlim, has_flex_unlim, is_pro_veo3_available, cohort,
			subscription_balance, credits_balance, total_plan_credits, plan_ends_at,
			status, in_flight_jobs, last_balance_at, last_used_at, last_failed_at, fail_streak,
			bound_proxy_url, registered_at, imported_at
		) VALUES (
			?, ?, ?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?
		)
		ON CONFLICT(id) DO UPDATE SET
			email = excluded.email,
			password_enc = excluded.password_enc,
			session_id = excluded.session_id,
			cookies_json = excluded.cookies_json,
			user_agent = excluded.user_agent,
			datadome_client_id = excluded.datadome_client_id,
			workspace_id = excluded.workspace_id,
			plan_type = excluded.plan_type,
			has_unlim = excluded.has_unlim,
			has_flex_unlim = excluded.has_flex_unlim,
			is_pro_veo3_available = excluded.is_pro_veo3_available,
			cohort = excluded.cohort,
			subscription_balance = excluded.subscription_balance,
			credits_balance = excluded.credits_balance,
			total_plan_credits = excluded.total_plan_credits,
			plan_ends_at = excluded.plan_ends_at,
			status = excluded.status,
			bound_proxy_url = excluded.bound_proxy_url
	`,
		a.ID, a.Email, a.Password, a.SessionID, a.CookiesJSON, a.UserAgent,
		a.DataDomeClientID, a.WorkspaceID, string(a.PlanType),
		boolToInt(a.HasUnlim), boolToInt(a.HasFlexUnlim), boolToInt(a.IsProVeo3Available), a.Cohort,
		a.SubscriptionBalance, a.CreditsBalance, a.TotalPlanCredits, fmtTime(a.PlanEndsAt),
		string(a.Status), a.InFlightJobs, fmtTime(a.LastBalanceAt), fmtTime(a.LastUsedAt), fmtTime(a.LastFailedAt), a.FailStreak,
		a.BoundProxyURL, fmtTime(a.RegisteredAt), fmtTime(a.ImportedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert account %s: %w", a.ID, err)
	}
	return nil
}

// UpdateBalance overwrites the balance triplet.
func (s *AccountStore) UpdateBalance(ctx context.Context, id string, sub, credits, pkg int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET subscription_balance = ?, credits_balance = ?, total_plan_credits = ?, last_balance_at = ?
		WHERE id = ?`,
		sub, credits, pkg, fmtTime(time.Now()), id)
	return err
}

// UpdateInFlight adjusts the in_flight_jobs counter atomically.
func (s *AccountStore) UpdateInFlight(ctx context.Context, id string, delta int) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET in_flight_jobs = MAX(0, in_flight_jobs + ?)
		WHERE id = ?`, delta, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAccountNotFound
	}
	return nil
}

// MarkStatus updates the lifecycle status field. When status is not "active",
// the fail_streak is incremented; on transition back to "active" it resets.
func (s *AccountStore) MarkStatus(ctx context.Context, id string, status domain.AccountStatus, reason string) error {
	var q string
	switch status {
	case domain.StatusActive:
		q = `UPDATE accounts SET status = ?, fail_streak = 0 WHERE id = ?`
	default:
		q = `UPDATE accounts SET status = ?, fail_streak = fail_streak + 1, last_failed_at = ? WHERE id = ?`
	}
	var (
		res sql.Result
		err error
	)
	if status == domain.StatusActive {
		res, err = s.db.ExecContext(ctx, q, string(status), id)
	} else {
		res, err = s.db.ExecContext(ctx, q, string(status), fmtTime(time.Now()), id)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAccountNotFound
	}
	_ = reason // audit logging is emitted at the caller level for now.
	return nil
}

// PickAndLock is a naive implementation: it picks a single eligible account
// inside a transaction, increments in_flight_jobs, and returns.
//
// The lock token is currently just the account id — a future implementation
// backed by row versioning will use a real token.
func (s *AccountStore) PickAndLock(ctx context.Context, params ports.PickParams) (*domain.Account, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()

	// Baseline eligibility: active, has room for another job, has budget.
	// Plan tier gates are enforced in the WHERE clause when the caller
	// asks for a paid tier (RequiresPaid / RequiresUltra).
	q := strings.Builder{}
	args := []any{}
	q.WriteString(`
		SELECT id, email, password_enc, session_id, cookies_json, user_agent,
		       datadome_client_id, workspace_id, plan_type,
		       has_unlim, has_flex_unlim, is_pro_veo3_available, cohort,
		       subscription_balance, credits_balance, total_plan_credits, plan_ends_at,
		       status, in_flight_jobs, last_balance_at, last_used_at, last_failed_at, fail_streak,
		       bound_proxy_url, registered_at, imported_at
		FROM accounts
		WHERE status = 'active'
		  AND in_flight_jobs < 5
		  AND subscription_balance >= ?`)
	minBalance := params.EstCostHundredths + params.EstCostHundredths/5
	args = append(args, minBalance)

	if params.RequiresPaid {
		q.WriteString(` AND plan_type NOT IN ('free','starter','basic')`)
	}
	if params.RequiresUltra {
		q.WriteString(` AND plan_type IN ('ultra','ultimate','scale','creator','team','enterprise')`)
	}
	if params.RequiresUnlim {
		q.WriteString(` AND has_unlim = 1`)
	}

	// Group scoping.
	if params.GroupID != "" {
		q.WriteString(` AND id IN (SELECT account_id FROM account_group_members WHERE group_id = ?)`)
		args = append(args, params.GroupID)
	}

	// Ordering: least-recently-used first (round-robin default).
	q.WriteString(` ORDER BY COALESCE(last_used_at, '1970-01-01T00:00:00Z') ASC LIMIT 1`)

	row := tx.QueryRowContext(ctx, q.String(), args...)
	acc, err := scanAccount(row)
	if err != nil {
		// scanAccount already remaps sql.ErrNoRows → ErrAccountNotFound; in
		// the PickAndLock context "no rows" means "no candidate matched".
		if errors.Is(err, domain.ErrAccountNotFound) || errors.Is(err, sql.ErrNoRows) {
			return nil, "", domain.ErrNoEligibleAccount
		}
		return nil, "", err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE accounts SET in_flight_jobs = in_flight_jobs + 1, last_used_at = ? WHERE id = ?`,
		fmtTime(time.Now()), acc.ID); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	acc.InFlightJobs++
	acc.LastUsedAt = time.Now()
	return acc, acc.ID, nil
}

// Unlock decrements the in_flight counter. lockToken is currently the account id.
func (s *AccountStore) Unlock(ctx context.Context, id string, lockToken string) error {
	if id != lockToken {
		return fmt.Errorf("unlock: lock token mismatch (id=%s token=%s)", id, lockToken)
	}
	return s.UpdateInFlight(ctx, id, -1)
}

// scanAccount reads a single accounts row from either *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanAccount(sc scanner) (*domain.Account, error) {
	var (
		a             domain.Account
		planEndsAt    sql.NullString
		lastBalanceAt sql.NullString
		lastUsedAt    sql.NullString
		lastFailedAt  sql.NullString
		registeredAt  sql.NullString
		importedAt    sql.NullString
		datadomeCID   sql.NullString
		workspaceID   sql.NullString
		cohort        sql.NullString
		boundProxy    sql.NullString
		planType      string
		status        string
		hasUnlim      int
		hasFlexUnlim  int
		isProVeo3     int
	)
	if err := sc.Scan(
		&a.ID, &a.Email, &a.Password, &a.SessionID, &a.CookiesJSON, &a.UserAgent,
		&datadomeCID, &workspaceID, &planType,
		&hasUnlim, &hasFlexUnlim, &isProVeo3, &cohort,
		&a.SubscriptionBalance, &a.CreditsBalance, &a.TotalPlanCredits, &planEndsAt,
		&status, &a.InFlightJobs, &lastBalanceAt, &lastUsedAt, &lastFailedAt, &a.FailStreak,
		&boundProxy, &registeredAt, &importedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrAccountNotFound
		}
		return nil, err
	}
	a.PlanType = domain.PlanType(planType)
	a.Status = domain.AccountStatus(status)
	a.HasUnlim = intToBool(hasUnlim)
	a.HasFlexUnlim = intToBool(hasFlexUnlim)
	a.IsProVeo3Available = intToBool(isProVeo3)
	a.Cohort = cohort.String
	a.DataDomeClientID = datadomeCID.String
	a.WorkspaceID = workspaceID.String
	a.BoundProxyURL = boundProxy.String
	a.PlanEndsAt = parseTime(planEndsAt.String)
	a.LastBalanceAt = parseTime(lastBalanceAt.String)
	a.LastUsedAt = parseTime(lastUsedAt.String)
	a.LastFailedAt = parseTime(lastFailedAt.String)
	a.RegisteredAt = parseTime(registeredAt.String)
	a.ImportedAt = parseTime(importedAt.String)
	return &a, nil
}
