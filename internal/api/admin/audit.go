package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// AuditHandler serves /admin/audit endpoints. It exposes the append-only
// audit_events table written by the audit middleware on every /admin
// write. Read-only: this surface never mutates the table.
type AuditHandler struct {
	Audit ports.AuditStore
}

// NewAuditHandler wires an AuditHandler over the given store.
func NewAuditHandler(a ports.AuditStore) *AuditHandler {
	return &AuditHandler{Audit: a}
}

// Register mounts the routes under /admin/audit.
func (h *AuditHandler) Register(r chi.Router) {
	r.Get("/audit", h.List)
}

// defaultAuditLimit is the page size applied when the caller omits ?limit=.
const defaultAuditLimit = 100

// maxAuditLimit caps the caller-provided ?limit= so a single request cannot
// pull every row out of the table.
const maxAuditLimit = 500

// List serves GET /audit. Query params:
//
//	since / until      RFC3339 timestamps (falls back to unix seconds)
//	actor              filter by truncated bearer prefix or "anonymous"
//	resource_type      apikey / account / group / job / partner
//	resource_id        chi URL param value (e.g. an api key id)
//	method             POST / PUT / PATCH / DELETE
//	limit / offset     paging; limit defaults to 100 and caps at 500
func (h *AuditHandler) List(w http.ResponseWriter, r *http.Request) {
	filter, ok := parseAuditFilter(w, r)
	if !ok {
		return
	}
	rows, err := h.Audit.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, auditEventView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   data,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// parseAuditFilter pulls the filter/paging params from the request.
// On invalid input it writes a 400 and returns ok=false so the caller
// can bail without hitting the store.
func parseAuditFilter(w http.ResponseWriter, r *http.Request) (ports.AuditFilter, bool) {
	q := r.URL.Query()
	out := ports.AuditFilter{
		Actor:        q.Get("actor"),
		ResourceType: q.Get("resource_type"),
		ResourceID:   q.Get("resource_id"),
		Method:       q.Get("method"),
		Limit:        defaultAuditLimit,
	}
	if raw := q.Get("since"); raw != "" {
		t, err := parseAuditTimeParam(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "since: "+err.Error())
			return ports.AuditFilter{}, false
		}
		out.Since = t
	}
	if raw := q.Get("until"); raw != "" {
		t, err := parseAuditTimeParam(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "until: "+err.Error())
			return ports.AuditFilter{}, false
		}
		out.Until = t
	}
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_query", "limit must be a non-negative integer")
			return ports.AuditFilter{}, false
		}
		if v == 0 {
			v = defaultAuditLimit
		}
		if v > maxAuditLimit {
			v = maxAuditLimit
		}
		out.Limit = v
	}
	if raw := q.Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_query", "offset must be a non-negative integer")
			return ports.AuditFilter{}, false
		}
		out.Offset = v
	}
	return out, true
}

// parseAuditTimeParam accepts RFC3339 first, then unix seconds. Kept
// distinct from parseTimeParam in usage.go so the admin package stays
// free of cross-handler helper drift; both paths are trivial.
func parseAuditTimeParam(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Time{}, errInvalidAuditTime
}

// errInvalidAuditTime is a sentinel for parseAuditTimeParam so the
// message stays short and consistent across since / until.
var errInvalidAuditTime = &auditParseError{"expected RFC3339 or unix seconds"}

type auditParseError struct{ msg string }

func (e *auditParseError) Error() string { return e.msg }

// auditEventView is the JSON representation of an AuditEvent. Keys
// match the audit_events column names for parity with any operator
// tool that reads the DB directly.
func auditEventView(e *domain.AuditEvent) map[string]any {
	return map[string]any{
		"id":            e.ID,
		"ts":            e.TS.UTC().Format(time.RFC3339),
		"actor":         e.Actor,
		"method":        e.Method,
		"path":          e.Path,
		"route":         e.Route,
		"status":        e.Status,
		"resource_type": e.ResourceType,
		"resource_id":   e.ResourceID,
		"body_hash":     e.BodyHash,
		"error_detail":  e.ErrorDetail,
	}
}
