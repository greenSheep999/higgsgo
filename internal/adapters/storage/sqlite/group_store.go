package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// GroupStore implements ports.GroupStore backed by SQLite.
//
// Rows land in three tables (all created by migration 001_init.sql):
//
//   - account_groups: one row per group, with quotas and routing policy
//   - account_group_members: many-to-many between accounts and groups
//   - apikey_group_bindings: many-to-many between api keys and groups
//
// Membership and bindings use INSERT ... ON CONFLICT DO NOTHING so callers
// can retry safely; List* methods return sorted, deterministic slices.
type GroupStore struct {
	db *DB
}

// NewGroupStore returns a fresh GroupStore rooted at the given DB.
func NewGroupStore(db *DB) *GroupStore { return &GroupStore{db: db} }

// Get returns a single group row by id.
func (s *GroupStore) Get(ctx context.Context, id string) (*domain.Group, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, description,
		       max_concurrent_jobs, max_concurrent_per_account,
		       monthly_credit_budget, monthly_credit_used,
		       allowed_models_regex, blocked_models_regex,
		       route_strategy, owner_type, owner_id,
		       status, created_at
		FROM account_groups WHERE id = ?`, id)
	return scanGroup(row)
}

// GetByName returns a single group row by name (name is UNIQUE in the schema).
func (s *GroupStore) GetByName(ctx context.Context, name string) (*domain.Group, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, description,
		       max_concurrent_jobs, max_concurrent_per_account,
		       monthly_credit_budget, monthly_credit_used,
		       allowed_models_regex, blocked_models_regex,
		       route_strategy, owner_type, owner_id,
		       status, created_at
		FROM account_groups WHERE name = ?`, name)
	return scanGroup(row)
}

// Create inserts a new group. Caller supplies the id and required fields;
// zero-valued optional fields default to schema defaults.
func (s *GroupStore) Create(ctx context.Context, g *domain.Group) error {
	if g == nil {
		return errors.New("create group: nil group")
	}
	created := g.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	route := string(g.RouteStrategy)
	if route == "" {
		route = string(domain.RouteRoundRobin)
	}
	status := g.Status
	if status == "" {
		status = "active"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO account_groups (
			id, name, description,
			max_concurrent_jobs, max_concurrent_per_account,
			monthly_credit_budget, monthly_credit_used,
			allowed_models_regex, blocked_models_regex,
			route_strategy, owner_type, owner_id,
			status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.Name, nullStr(g.Description),
		nullInt(int64(g.MaxConcurrentJobs)), nullInt(int64(g.MaxConcurrentPerAccount)),
		nullInt(g.MonthlyCreditBudget), g.MonthlyCreditUsed,
		nullStr(g.AllowedModelsRegex), nullStr(g.BlockedModelsRegex),
		route, string(g.OwnerType), nullStr(g.OwnerID),
		status, fmtTime(created),
	)
	if err != nil {
		return fmt.Errorf("insert group %s: %w", g.ID, err)
	}
	return nil
}

// Update overwrites the mutable fields of an existing group (matched by id).
// monthly_credit_used is not modified here — use IncrementUsed for that so
// concurrent writers do not clobber each other.
func (s *GroupStore) Update(ctx context.Context, g *domain.Group) error {
	if g == nil {
		return errors.New("update group: nil group")
	}
	route := string(g.RouteStrategy)
	if route == "" {
		route = string(domain.RouteRoundRobin)
	}
	status := g.Status
	if status == "" {
		status = "active"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE account_groups SET
			name = ?,
			description = ?,
			max_concurrent_jobs = ?,
			max_concurrent_per_account = ?,
			monthly_credit_budget = ?,
			allowed_models_regex = ?,
			blocked_models_regex = ?,
			route_strategy = ?,
			owner_type = ?,
			owner_id = ?,
			status = ?
		WHERE id = ?`,
		g.Name, nullStr(g.Description),
		nullInt(int64(g.MaxConcurrentJobs)), nullInt(int64(g.MaxConcurrentPerAccount)),
		nullInt(g.MonthlyCreditBudget),
		nullStr(g.AllowedModelsRegex), nullStr(g.BlockedModelsRegex),
		route, string(g.OwnerType), nullStr(g.OwnerID),
		status,
		g.ID,
	)
	if err != nil {
		return fmt.Errorf("update group %s: %w", g.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrGroupNotFound
	}
	return nil
}

// Delete removes a group row. The ON DELETE CASCADE clauses on the member
// and binding tables clean up related rows automatically.
func (s *GroupStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM account_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete group %s: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrGroupNotFound
	}
	return nil
}

// List returns every group row, ordered by name for stable output.
func (s *GroupStore) List(ctx context.Context) ([]domain.Group, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, description,
		       max_concurrent_jobs, max_concurrent_per_account,
		       monthly_credit_budget, monthly_credit_used,
		       allowed_models_regex, blocked_models_regex,
		       route_strategy, owner_type, owner_id,
		       status, created_at
		FROM account_groups
		ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	var out []domain.Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// AddMember links an account to a group. The (group_id, account_id) pair is
// the primary key, so ON CONFLICT DO NOTHING makes the call idempotent —
// callers can safely retry without hitting a UNIQUE violation.
func (s *GroupStore) AddMember(ctx context.Context, groupID, accountID string, priority int) error {
	if priority == 0 {
		priority = 100
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO account_group_members (account_id, group_id, priority, added_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(account_id, group_id) DO UPDATE SET priority = excluded.priority`,
		accountID, groupID, priority, fmtTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("add member group=%s account=%s: %w", groupID, accountID, err)
	}
	return nil
}

// RemoveMember unlinks an account from a group. Returns nil even if the pair
// was not present so callers do not need to pre-check.
func (s *GroupStore) RemoveMember(ctx context.Context, groupID, accountID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM account_group_members WHERE group_id = ? AND account_id = ?`,
		groupID, accountID)
	if err != nil {
		return fmt.Errorf("remove member group=%s account=%s: %w", groupID, accountID, err)
	}
	return nil
}

// ListMembers returns the account ids belonging to the given group, ordered
// by priority DESC then account_id ASC for deterministic output.
func (s *GroupStore) ListMembers(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT account_id FROM account_group_members
		WHERE group_id = ?
		ORDER BY priority DESC, account_id ASC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("list members group=%s: %w", groupID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// BindAPIKey allows the given api key to consume accounts from the given
// group. Uses ON CONFLICT DO NOTHING so the call is idempotent.
func (s *GroupStore) BindAPIKey(ctx context.Context, apiKeyID, groupID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO apikey_group_bindings (api_key_id, group_id)
		VALUES (?, ?)
		ON CONFLICT(api_key_id, group_id) DO NOTHING`,
		apiKeyID, groupID)
	if err != nil {
		return fmt.Errorf("bind apikey=%s group=%s: %w", apiKeyID, groupID, err)
	}
	return nil
}

// UnbindAPIKey removes an api-key-to-group binding. Returns nil if no such
// row exists.
func (s *GroupStore) UnbindAPIKey(ctx context.Context, apiKeyID, groupID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM apikey_group_bindings WHERE api_key_id = ? AND group_id = ?`,
		apiKeyID, groupID)
	if err != nil {
		return fmt.Errorf("unbind apikey=%s group=%s: %w", apiKeyID, groupID, err)
	}
	return nil
}

// ListGroupsForAPIKey returns every group the given api key is bound to,
// ordered by group name for deterministic output.
func (s *GroupStore) ListGroupsForAPIKey(ctx context.Context, apiKeyID string) ([]domain.Group, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.name, g.description,
		       g.max_concurrent_jobs, g.max_concurrent_per_account,
		       g.monthly_credit_budget, g.monthly_credit_used,
		       g.allowed_models_regex, g.blocked_models_regex,
		       g.route_strategy, g.owner_type, g.owner_id,
		       g.status, g.created_at
		FROM account_groups g
		INNER JOIN apikey_group_bindings b ON b.group_id = g.id
		WHERE b.api_key_id = ?
		ORDER BY g.name ASC`, apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("list groups for apikey=%s: %w", apiKeyID, err)
	}
	defer rows.Close()

	var out []domain.Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *g)
	}
	return out, rows.Err()
}

// IncrementUsed atomically adds deltaHundredths to the group's
// monthly_credit_used counter. The metering pipeline calls this after each
// terminal job so operators can enforce group-scoped budgets.
func (s *GroupStore) IncrementUsed(ctx context.Context, groupID string, deltaHundredths int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE account_groups
		SET monthly_credit_used = monthly_credit_used + ?
		WHERE id = ?`, deltaHundredths, groupID)
	if err != nil {
		return fmt.Errorf("increment used group=%s: %w", groupID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.ErrGroupNotFound
	}
	return nil
}

// CurrentInFlight returns the aggregate in_flight_jobs across every account
// that is a member of the given group. The pool picker uses this to enforce
// Group.MaxConcurrentJobs at pick time.
func (s *GroupStore) CurrentInFlight(ctx context.Context, groupID string) (int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(a.in_flight_jobs), 0)
		FROM accounts a
		INNER JOIN account_group_members m ON m.account_id = a.id
		WHERE m.group_id = ?`, groupID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("current in-flight group=%s: %w", groupID, err)
	}
	return n, nil
}

// scanGroup reads a single account_groups row from either *sql.Row or *sql.Rows.
func scanGroup(sc scanner) (*domain.Group, error) {
	var (
		g            domain.Group
		description  sql.NullString
		maxConcJobs  sql.NullInt64
		maxConcAcct  sql.NullInt64
		monthlyBudg  sql.NullInt64
		allowedRegex sql.NullString
		blockedRegex sql.NullString
		ownerID      sql.NullString
		route        string
		ownerType    string
		createdAt    string
	)
	if err := sc.Scan(
		&g.ID, &g.Name, &description,
		&maxConcJobs, &maxConcAcct,
		&monthlyBudg, &g.MonthlyCreditUsed,
		&allowedRegex, &blockedRegex,
		&route, &ownerType, &ownerID,
		&g.Status, &createdAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrGroupNotFound
		}
		return nil, err
	}
	g.Description = description.String
	g.MaxConcurrentJobs = int(maxConcJobs.Int64)
	g.MaxConcurrentPerAccount = int(maxConcAcct.Int64)
	g.MonthlyCreditBudget = monthlyBudg.Int64
	g.AllowedModelsRegex = allowedRegex.String
	g.BlockedModelsRegex = blockedRegex.String
	g.RouteStrategy = domain.RouteStrategy(strings.TrimSpace(route))
	g.OwnerType = domain.OwnerType(ownerType)
	g.OwnerID = ownerID.String
	g.CreatedAt = parseTime(createdAt)
	return &g, nil
}
