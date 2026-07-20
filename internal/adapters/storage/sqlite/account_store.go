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

// accountColumns is the canonical SELECT list used by every reader on
// the accounts table. Kept as a package-level constant so a schema
// extension only needs a single edit rather than N string-literal
// duplicates.
const accountColumns = `id, email, password_enc, session_id, cookies_json, user_agent,
		datadome_client_id, workspace_id, plan_type,
		has_unlim, has_flex_unlim, is_pro_veo3_available, cohort,
		subscription_balance, credits_balance, total_plan_credits, plan_ends_at,
		status, in_flight_jobs, last_balance_at, last_used_at, last_failed_at, fail_streak,
		bound_proxy_url, priority, throttled_until, status_reason,
		max_concurrent, note, source,
		registered_at, imported_at`

// Get returns a single account by id.
func (s *AccountStore) Get(ctx context.Context, id string) (*domain.Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id)
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

	q := `SELECT ` + accountColumns + ` FROM accounts`
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
//
// throttled_until / status_reason are intentionally NOT touched by the
// UPDATE branch — the failover controller owns those two columns and a
// full-account import should not clobber an in-progress cooldown or the
// last-recorded reason on an existing row. New rows still land with the
// caller-provided values so an import can restore state on a fresh DB.
func (s *AccountStore) Upsert(ctx context.Context, a *domain.Account) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO accounts (
			id, email, password_enc, session_id, cookies_json, user_agent,
			datadome_client_id, workspace_id, plan_type,
			has_unlim, has_flex_unlim, is_pro_veo3_available, cohort,
			subscription_balance, credits_balance, total_plan_credits, plan_ends_at,
			status, in_flight_jobs, last_balance_at, last_used_at, last_failed_at, fail_streak,
			bound_proxy_url, priority, throttled_until, status_reason,
			max_concurrent, note, source,
			registered_at, imported_at
		) VALUES (
			?, ?, ?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?
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
			bound_proxy_url = excluded.bound_proxy_url,
			priority = excluded.priority,
			max_concurrent = excluded.max_concurrent,
			note = excluded.note,
			source = excluded.source
	`,
		a.ID, a.Email, a.Password, a.SessionID, a.CookiesJSON, a.UserAgent,
		a.DataDomeClientID, a.WorkspaceID, string(a.PlanType),
		boolToInt(a.HasUnlim), boolToInt(a.HasFlexUnlim), boolToInt(a.IsProVeo3Available), a.Cohort,
		a.SubscriptionBalance, a.CreditsBalance, a.TotalPlanCredits, fmtTime(a.PlanEndsAt),
		string(a.Status), a.InFlightJobs, fmtTime(a.LastBalanceAt), fmtTime(a.LastUsedAt), fmtTime(a.LastFailedAt), a.FailStreak,
		a.BoundProxyURL, a.Priority, nullableTime(a.ThrottledUntil), a.StatusReason,
		a.MaxConcurrent, a.Note, a.Source,
		fmtTime(a.RegisteredAt), fmtTime(a.ImportedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert account %s: %w", a.ID, err)
	}
	return nil
}

// nullableTime returns a sql-friendly value for a nullable TEXT
// timestamp column: an interface holding nil for a zero time.Time (so
// the column stores NULL) or the RFC3339 string otherwise. Used for
// accounts.throttled_until where the "no cooldown" state must be a
// real NULL for the PickAndLock filter to short-circuit cleanly.
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeFormat)
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

// UpdateEntitlements refreshes the API-side permission flags observed via
// GET /user. Balances live in a separate UpdateBalance call so the two
// endpoints (which come from the refresher goroutine) do not step on each
// other's writes.
func (s *AccountStore) UpdateEntitlements(ctx context.Context, id string, e ports.EntitlementUpdate) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET plan_type = ?,
		    has_unlim = ?,
		    has_flex_unlim = ?,
		    is_pro_veo3_available = ?,
		    cohort = ?,
		    total_plan_credits = ?,
		    plan_ends_at = ?
		WHERE id = ?`,
		string(e.PlanType),
		boolToInt(e.HasUnlim),
		boolToInt(e.HasFlexUnlim),
		boolToInt(e.IsProVeo3Available),
		e.Cohort,
		e.TotalPlanCredits,
		fmtTime(e.PlanEndsAt),
		id,
	)
	if err != nil {
		return fmt.Errorf("update entitlements %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAccountNotFound
	}
	return nil
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

// ResetAllInFlight zeroes in_flight_jobs across the whole accounts table
// and returns the count of rows that were previously > 0. Called from
// main.go once at boot as a safety net against counter leaks from a
// prior crash between PickAndLock and Unlock — without it, a killed
// process leaves the row's slot permanently consumed until an operator
// manually intervenes. Safe: any real in-flight jobs from before the
// crash are dead upstream anyway (no goroutine is polling them).
func (s *AccountStore) ResetAllInFlight(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET in_flight_jobs = 0
		WHERE in_flight_jobs > 0`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MarkStatus updates the lifecycle status field and persists the reason
// opcode. When status is StatusActive, the transition also clears the
// throttled_until column and the status_reason string so the row starts
// clean on the recovery edge (operator "recover disabled" flow or the
// Recoverer flip). fail_streak is preserved on active transitions
// because the failover controller owns it separately via
// ResetFailStreak — resetting here would silently mask misuse.
//
// For every non-active status the call bumps fail_streak, stamps
// last_failed_at, and writes the reason. The failover controller
// prefers the finer-grained IncrFailStreak for tick-only updates and
// only calls MarkStatus on the terminal edge (evict / disable).
func (s *AccountStore) MarkStatus(ctx context.Context, id string, status domain.AccountStatus, reason string) error {
	var (
		res sql.Result
		err error
	)
	switch status {
	case domain.StatusActive:
		res, err = s.db.ExecContext(ctx, `
			UPDATE accounts
			SET status = ?, status_reason = ?, throttled_until = NULL
			WHERE id = ?`,
			string(status), reason, id)
	default:
		res, err = s.db.ExecContext(ctx, `
			UPDATE accounts
			SET status = ?,
			    status_reason = ?,
			    fail_streak = fail_streak + 1,
			    last_failed_at = ?
			WHERE id = ?`,
			string(status), reason, fmtTime(time.Now()), id)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAccountNotFound
	}
	return nil
}

// MarkThrottled parks the account in status=throttled and stamps
// throttled_until with the caller-supplied deadline. Written by the
// failover controller (mechanism ②) when the sliding-window judge
// count is reached. Does not touch fail_streak — throttle and
// consecutive-fail streaks are independent signals; a throttled
// account that later returns 401 still ticks the streak.
func (s *AccountStore) MarkThrottled(ctx context.Context, id string, until time.Time, reason string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET status = ?,
		    status_reason = ?,
		    throttled_until = ?,
		    last_failed_at = ?
		WHERE id = ?`,
		string(domain.StatusThrottled), reason, nullableTime(until), fmtTime(time.Now()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAccountNotFound
	}
	return nil
}

// RecoverThrottled bulk-flips every throttled row whose cooldown has
// expired back to status=active, clears throttled_until, and blanks
// status_reason so the row is indistinguishable from an untouched
// active row afterwards. Returns the number of rows recovered.
//
// Runs on a fixed cadence from core/failover.Recoverer; a naive UPDATE
// keeps the operation single-SQL so contention with an in-flight
// PickAndLock is bounded to the SQLite busy_timeout.
func (s *AccountStore) RecoverThrottled(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE accounts
		SET status = ?,
		    throttled_until = NULL,
		    status_reason = ''
		WHERE status = ? AND throttled_until IS NOT NULL AND throttled_until <= ?`,
		string(domain.StatusActive), string(domain.StatusThrottled), fmtTime(time.Now()))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// IncrFailStreak atomically increments fail_streak, stamps
// last_failed_at, and returns the resulting streak count. Preferred
// over MarkStatus for tick-only writes because it does not flip status
// (the failover controller decides whether the new streak is over the
// limit) and does not overwrite status_reason.
//
// Returns ErrAccountNotFound when the id does not exist.
func (s *AccountStore) IncrFailStreak(ctx context.Context, id string) (int, error) {
	// SQLite's UPDATE ... RETURNING landed in 3.35 — the modernc.org/sqlite
	// driver bundled with higgsgo speaks it. Fall back to a read after the
	// update if a future driver revert breaks this.
	row := s.db.QueryRowContext(ctx, `
		UPDATE accounts
		SET fail_streak = fail_streak + 1, last_failed_at = ?
		WHERE id = ?
		RETURNING fail_streak`,
		fmtTime(time.Now()), id)
	var streak int
	if err := row.Scan(&streak); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, domain.ErrAccountNotFound
		}
		return 0, err
	}
	return streak, nil
}

// ResetFailStreak zeroes fail_streak. Called on every successful
// upstream outcome by the failover controller.
func (s *AccountStore) ResetFailStreak(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE accounts SET fail_streak = 0 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrAccountNotFound
	}
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

	// Group-aggregate concurrency check runs first: if the group's
	// MaxConcurrentJobs is set and the current SUM(in_flight_jobs) has
	// already reached it, return ErrGroupConcurrencyMax without scanning
	// candidate rows. Runs inside the same transaction as the account
	// SELECT+UPDATE so two concurrent Pick calls cannot both slip past
	// the check (SQLite serializes writers). See docs/ROADMAP.md P0-3.
	if params.GroupID != "" && params.MaxGroupInFlight > 0 {
		var inFlight int
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(a.in_flight_jobs), 0)
			FROM accounts a
			JOIN account_group_members m ON m.account_id = a.id
			WHERE m.group_id = ?`, params.GroupID).Scan(&inFlight)
		if err != nil {
			return nil, "", err
		}
		if inFlight >= params.MaxGroupInFlight {
			return nil, "", domain.ErrGroupConcurrencyMax
		}
	}

	// Per-account cap precedence (F4 fix — 2026-07-20 audit):
	//   1. accounts.max_concurrent column, when > 0, is the tightest
	//      operator-facing knob (visible in WebUI, editable per-row).
	//   2. ports.PickParams.MaxConcurrentPerAccount is the group-scoped
	//      override callers pass in.
	//   3. A hard fallback of 5 catches deployments where neither the
	//      column nor the caller supplied a value — preserves the
	//      pre-F4 behaviour.
	//
	// The account-column check is inlined into the WHERE clause below so
	// SQLite can plan against `idx_accounts_pool` without needing a
	// second CTE. The Go-side variable here carries the group/fallback
	// cap; the SQL applies both bounds via AND, effectively an implicit
	// MIN().
	perAccountCap := params.MaxConcurrentPerAccount
	if perAccountCap <= 0 {
		perAccountCap = 5
	}

	// Baseline eligibility: active, has room for another job, has budget.
	// Plan tier gates are enforced in the WHERE clause when the caller
	// asks for a paid tier (RequiresPaid / RequiresUltra).
	q := strings.Builder{}
	args := []any{}
	// Eligibility is either "already active" or "throttled but the
	// cooldown deadline has passed" (a lazy fallback so a Recoverer
	// stall does not silently starve the pool). The Recoverer
	// goroutine promotes the throttled row on its next tick.
	//
	// The `(max_concurrent = 0 OR in_flight_jobs < max_concurrent)`
	// predicate enforces the account-column cap when the operator set
	// one; a zero (the default) skips the check so untouched rows keep
	// the group/fallback semantics unchanged.
	q.WriteString(`
		SELECT ` + accountColumns + `
		FROM accounts
		WHERE (
		        status = 'active'
		        OR (status = 'throttled' AND throttled_until IS NOT NULL AND throttled_until <= ?)
		      )
		  AND in_flight_jobs < ?
		  AND (max_concurrent = 0 OR in_flight_jobs < max_concurrent)
		  AND subscription_balance >= ?`)
	args = append(args, fmtTime(time.Now()))
	args = append(args, perAccountCap)
	minBalance := params.EstCostHundredths + params.EstCostHundredths/5
	args = append(args, minBalance)

	if params.RequiresPaid {
		q.WriteString(` AND plan_type NOT IN ('free','starter','basic')`)
	}
	if params.RequiresUltra {
		q.WriteString(` AND plan_type IN ('ultra','ultimate','scale','creator','team','enterprise')`)
	}
	if minRank := params.MinPlan.TierRank(); minRank > 0 {
		q.WriteString(` AND CASE plan_type
			WHEN 'starter' THEN 1
			WHEN 'basic' THEN 2
			WHEN 'pro' THEN 3
			WHEN 'plus' THEN 4
			WHEN 'ultra' THEN 5
			WHEN 'ultimate' THEN 5
			WHEN 'scale' THEN 5
			WHEN 'creator' THEN 5
			WHEN 'team' THEN 5
			WHEN 'enterprise' THEN 5
			ELSE 0
		END >= ?`)
		args = append(args, minRank)
	}
	if params.RequiresUnlim {
		q.WriteString(` AND has_unlim = 1`)
	}

	// Group scoping.
	if params.GroupID != "" {
		q.WriteString(` AND id IN (SELECT account_id FROM account_group_members WHERE group_id = ?)`)
		args = append(args, params.GroupID)
	}

	// Ordering: determined by RouteStrategy.
	//
	// Every strategy appends a common `in_flight_jobs ASC, RANDOM()`
	// tail (ROADMAP P2-8). Rationale: the primary sort key ties often
	// under real load — several accounts share the same LRU stamp
	// (identical last_used_at truncated to seconds), the same
	// plan_type, or the same priority. Without a tiebreaker SQLite's
	// natural row order made the same row win repeatedly for a burst
	// of concurrent picks, producing a hot-spot until last_used_at
	// finally advanced. The tail:
	//   1. `in_flight_jobs ASC` — prefer the least-loaded account
	//      first, so concurrent picks spread rather than pile up. This
	//      is the cheap fair-share primitive the audit called for.
	//   2. `RANDOM()` — final tiebreaker; two picks with identical
	//      metadata land on different rows probabilistically.
	// Runs under LIMIT 1 so RANDOM()'s per-row eval is O(N) at pool
	// size — hundreds of rows at most, sub-millisecond in practice.
	strategy := params.RouteStrategy
	if strategy == "" {
		strategy = domain.RouteRoundRobin
	}
	const jitterTail = `, in_flight_jobs ASC, RANDOM() LIMIT 1`
	switch strategy {
	case domain.RouteLeastUsed:
		// Account with the highest remaining balance has consumed the fewest credits.
		q.WriteString(` ORDER BY (COALESCE(total_plan_credits, 0) - COALESCE(subscription_balance, 0)) ASC, COALESCE(last_used_at, '1970-01-01T00:00:00Z') ASC` + jitterTail)
	case domain.RouteCheapestFirst:
		// Prefer lower-tier plans so expensive plan budgets are reserved for
		// models that require them. Numeric tier: lower = cheaper.
		q.WriteString(` ORDER BY CASE plan_type`)
		q.WriteString(` WHEN 'free' THEN 1`)
		q.WriteString(` WHEN 'starter' THEN 2`)
		q.WriteString(` WHEN 'basic' THEN 3`)
		q.WriteString(` WHEN 'pro' THEN 4`)
		q.WriteString(` WHEN 'plus' THEN 5`)
		q.WriteString(` WHEN 'creator' THEN 6`)
		q.WriteString(` WHEN 'team' THEN 7`)
		q.WriteString(` WHEN 'scale' THEN 8`)
		q.WriteString(` WHEN 'ultimate' THEN 9`)
		q.WriteString(` WHEN 'ultra' THEN 10`)
		q.WriteString(` WHEN 'enterprise' THEN 11`)
		q.WriteString(` ELSE 99 END ASC, COALESCE(last_used_at, '1970-01-01T00:00:00Z') ASC` + jitterTail)
	case domain.RouteMostCreditsFirst:
		// Prefer the account with the largest remaining balance.
		q.WriteString(` ORDER BY (COALESCE(subscription_balance, 0) + COALESCE(credits_balance, 0)) DESC, COALESCE(last_used_at, '1970-01-01T00:00:00Z') ASC` + jitterTail)
	case domain.RoutePriority:
		// Use group-level priority from account_group_members when scoped to a group,
		// otherwise fall back to accounts.priority.
		if params.GroupID != "" {
			q.WriteString(` ORDER BY COALESCE((SELECT m.priority FROM account_group_members m WHERE m.account_id = accounts.id AND m.group_id = ?), 0) DESC, COALESCE(last_used_at, '1970-01-01T00:00:00Z') ASC` + jitterTail)
			args = append(args, params.GroupID)
		} else {
			q.WriteString(` ORDER BY COALESCE(priority, 0) DESC, COALESCE(last_used_at, '1970-01-01T00:00:00Z') ASC` + jitterTail)
		}
	default: // RouteRoundRobin — the "load_balance" default.
		// Composed ordering, no operator knobs:
		//   1. plan_tier_rank ASC  — cheap-first: burn the tier closest
		//      to the model's min_plan floor. Preserves the higher-tier
		//      account budgets for models that actually need them.
		//   2. last_used_at ASC    — LRU inside the same tier so a burst
		//      of requests on identical-plan accounts spreads out.
		//   3. in_flight_jobs ASC  — least-loaded next.
		//   4. RANDOM()            — jitter tiebreak.
		//
		// Rank table mirrors PlanType.TierRank() and the CASE used by
		// the MinPlan WHERE gate: starter=1, basic=2, pro=3, plus=4,
		// ultra-family=5, free=0. When the WHERE clause already applied
		// a MinPlan floor, this ORDER BY only sorts among survivors.
		q.WriteString(` ORDER BY CASE plan_type`)
		q.WriteString(` WHEN 'starter' THEN 1`)
		q.WriteString(` WHEN 'basic' THEN 2`)
		q.WriteString(` WHEN 'pro' THEN 3`)
		q.WriteString(` WHEN 'plus' THEN 4`)
		q.WriteString(` WHEN 'ultra' THEN 5`)
		q.WriteString(` WHEN 'ultimate' THEN 5`)
		q.WriteString(` WHEN 'scale' THEN 5`)
		q.WriteString(` WHEN 'creator' THEN 5`)
		q.WriteString(` WHEN 'team' THEN 5`)
		q.WriteString(` WHEN 'enterprise' THEN 5`)
		q.WriteString(` ELSE 0 END ASC, COALESCE(last_used_at, '1970-01-01T00:00:00Z') ASC` + jitterTail)
	}

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
		a              domain.Account
		planEndsAt     sql.NullString
		lastBalanceAt  sql.NullString
		lastUsedAt     sql.NullString
		lastFailedAt   sql.NullString
		throttledUntil sql.NullString
		statusReason   sql.NullString
		registeredAt   sql.NullString
		importedAt     sql.NullString
		datadomeCID    sql.NullString
		workspaceID    sql.NullString
		cohort         sql.NullString
		boundProxy     sql.NullString
		planType       string
		status         string
		hasUnlim       int
		hasFlexUnlim   int
		isProVeo3      int
	)
	if err := sc.Scan(
		&a.ID, &a.Email, &a.Password, &a.SessionID, &a.CookiesJSON, &a.UserAgent,
		&datadomeCID, &workspaceID, &planType,
		&hasUnlim, &hasFlexUnlim, &isProVeo3, &cohort,
		&a.SubscriptionBalance, &a.CreditsBalance, &a.TotalPlanCredits, &planEndsAt,
		&status, &a.InFlightJobs, &lastBalanceAt, &lastUsedAt, &lastFailedAt, &a.FailStreak,
		&boundProxy, &a.Priority, &throttledUntil, &statusReason,
		&a.MaxConcurrent, &a.Note, &a.Source,
		&registeredAt, &importedAt,
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
	a.ThrottledUntil = parseTime(throttledUntil.String)
	a.StatusReason = statusReason.String
	a.RegisteredAt = parseTime(registeredAt.String)
	a.ImportedAt = parseTime(importedAt.String)
	return &a, nil
}
