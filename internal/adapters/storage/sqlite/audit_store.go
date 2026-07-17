package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// AuditStore implements ports.AuditStore backed by the audit_events
// table added in migration 007.
//
// The audit middleware calls Insert in a background goroutine on every
// admin write; the /admin/audit list handler calls List to serve the
// operator view. There is no update / delete surface — the table is
// intentionally append-only.
type AuditStore struct {
	db *DB
}

// NewAuditStore returns a fresh AuditStore rooted at the given DB.
func NewAuditStore(db *DB) *AuditStore { return &AuditStore{db: db} }

// defaultAuditLimit is applied when the caller omits Limit or passes 0.
const defaultAuditLimit = 100

// maxAuditLimit caps the caller-provided Limit so a single call cannot
// pull every row out of the table.
const maxAuditLimit = 500

// Insert writes a single audit_events row.
func (s *AuditStore) Insert(ctx context.Context, e *domain.AuditEvent) error {
	if e == nil {
		return errors.New("insert audit event: nil event")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (
			id, ts, actor, method, path, route,
			status, resource_type, resource_id, body_hash, error_detail
		) VALUES (
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?
		)`,
		e.ID, fmtTime(e.TS), e.Actor, e.Method, e.Path, e.Route,
		e.Status, e.ResourceType, e.ResourceID, e.BodyHash, e.ErrorDetail,
	)
	if err != nil {
		return fmt.Errorf("insert audit event %s: %w", e.ID, err)
	}
	return nil
}

// List returns audit_events rows matching the filter, newest first.
func (s *AuditStore) List(ctx context.Context, filter ports.AuditFilter) ([]domain.AuditEvent, error) {
	clauses, args := buildAuditFilterClauses(filter)

	sqlStr := `SELECT id, ts, actor, method, path, route,
	                  status, resource_type, resource_id, body_hash, error_detail
	           FROM audit_events`
	if len(clauses) > 0 {
		sqlStr += " WHERE " + strings.Join(clauses, " AND ")
	}
	sqlStr += " ORDER BY ts DESC"

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}
	sqlStr += fmt.Sprintf(" LIMIT %d", limit)
	if filter.Offset > 0 {
		sqlStr += fmt.Sprintf(" OFFSET %d", filter.Offset)
	}

	rows, err := s.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()

	var out []domain.AuditEvent
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// buildAuditFilterClauses converts an AuditFilter into WHERE fragments
// and their positional args. Kept separate from List so both the
// production path and future callers (aggregation, exports) can reuse
// the same filter grammar.
func buildAuditFilterClauses(f ports.AuditFilter) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	if !f.Since.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, fmtTime(f.Since))
	}
	if !f.Until.IsZero() {
		clauses = append(clauses, "ts < ?")
		args = append(args, fmtTime(f.Until))
	}
	if f.Actor != "" {
		clauses = append(clauses, "actor = ?")
		args = append(args, f.Actor)
	}
	if f.ResourceType != "" {
		clauses = append(clauses, "resource_type = ?")
		args = append(args, f.ResourceType)
	}
	if f.ResourceID != "" {
		clauses = append(clauses, "resource_id = ?")
		args = append(args, f.ResourceID)
	}
	if f.Method != "" {
		clauses = append(clauses, "method = ?")
		args = append(args, f.Method)
	}
	return clauses, args
}

// scanAuditEvent reads one audit_events row into a domain.AuditEvent.
func scanAuditEvent(sc scanner) (*domain.AuditEvent, error) {
	var (
		e  domain.AuditEvent
		ts string
	)
	if err := sc.Scan(
		&e.ID, &ts, &e.Actor, &e.Method, &e.Path, &e.Route,
		&e.Status, &e.ResourceType, &e.ResourceID, &e.BodyHash, &e.ErrorDetail,
	); err != nil {
		return nil, err
	}
	e.TS = parseTime(ts)
	return &e, nil
}
