package admin

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
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
	r.Get("/audit/export", h.Export)
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

// parseAuditFilter pulls the filter/paging params from the request for
// the paginated List endpoint. Limit defaults to 100 and caps at 500.
// On invalid input it writes a 400 and returns ok=false so the caller
// can bail without hitting the store.
func parseAuditFilter(w http.ResponseWriter, r *http.Request) (ports.AuditFilter, bool) {
	out, ok := parseAuditFilterBase(w, r)
	if !ok {
		return ports.AuditFilter{}, false
	}
	if out.Limit == 0 {
		out.Limit = defaultAuditLimit
	}
	if out.Limit > maxAuditLimit {
		out.Limit = maxAuditLimit
	}
	return out, true
}

// parseAuditExportFilter pulls the filter/paging params for the streaming
// export endpoint. Unlike parseAuditFilter, an omitted ?limit= means
// "unlimited" (0) so callers can stream the whole table. A caller-provided
// limit is preserved verbatim — the export handler batches internally so
// large caps like ?limit=100000 do not need to fit in a single List call.
func parseAuditExportFilter(w http.ResponseWriter, r *http.Request) (ports.AuditFilter, bool) {
	return parseAuditFilterBase(w, r)
}

// parseAuditFilterBase shares the query-string parsing between the List
// and Export handlers. Limit is passed through verbatim (0 when omitted);
// individual callers apply their own default / cap semantics.
func parseAuditFilterBase(w http.ResponseWriter, r *http.Request) (ports.AuditFilter, bool) {
	q := r.URL.Query()
	out := ports.AuditFilter{
		Actor:        q.Get("actor"),
		ResourceType: q.Get("resource_type"),
		ResourceID:   q.Get("resource_id"),
		Method:       q.Get("method"),
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

// auditExportBatchSize is the page size used when the export handler
// walks the audit_events table. Matches the underlying store cap so
// each round-trip returns at most one full page.
const auditExportBatchSize = 500

// auditExportCSVHeader is the ordered column header written as the first
// row of a CSV export. Keys mirror auditEventView / audit_events.
var auditExportCSVHeader = []string{
	"id", "ts", "actor", "method", "path", "route",
	"status", "resource_type", "resource_id", "body_hash", "error_detail",
}

// Export serves GET /audit/export. Streams the audit_events table in
// either JSONL (default) or CSV format. Accepts the same filter params
// as List (since / until / actor / resource_type / resource_id / method /
// limit / offset); an omitted ?limit= means "stream everything" so
// auditors can pull an unbounded range in one download.
//
// Rows are fetched in batches of auditExportBatchSize and flushed to the
// wire as each batch completes so a large export does not sit in memory
// or stall the client for minutes before the first byte.
func (h *AuditHandler) Export(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "jsonl"
	}
	if format != "jsonl" && format != "csv" {
		writeErr(w, http.StatusBadRequest, "invalid_query", "format must be one of: jsonl, csv")
		return
	}

	filter, ok := parseAuditExportFilter(w, r)
	if !ok {
		return
	}

	// Fetch the first batch before writing headers so an early store
	// failure can still surface as a proper HTTP error.
	first, err := h.fetchAuditBatch(r, filter, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	ext := format
	contentType := "application/x-ndjson"
	if format == "csv" {
		contentType = "text/csv; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "attachment; filename="+auditExportFilename(filter, ext))
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	switch format {
	case "csv":
		cw := csv.NewWriter(w)
		_ = cw.Write(auditExportCSVHeader)
		if err := streamAuditBatches(r, filter, first, func(e *domain.AuditEvent) error {
			return cw.Write(auditEventCSVRow(e))
		}, func() {
			cw.Flush()
			if flusher != nil {
				flusher.Flush()
			}
		}, h.fetchAuditBatch); err != nil {
			// Headers are already committed — best we can do is stop
			// writing. The truncated stream is the signal to the client.
			return
		}
		cw.Flush()
		if flusher != nil {
			flusher.Flush()
		}
	case "jsonl":
		enc := json.NewEncoder(w)
		if err := streamAuditBatches(r, filter, first, func(e *domain.AuditEvent) error {
			return enc.Encode(auditEventView(e))
		}, func() {
			if flusher != nil {
				flusher.Flush()
			}
		}, h.fetchAuditBatch); err != nil {
			return
		}
	}
}

// fetchAuditBatch pulls one page of rows starting at filter.Offset+extra.
// The batch size honours both the underlying store cap and any total
// limit the caller supplied via ?limit=.
func (h *AuditHandler) fetchAuditBatch(r *http.Request, filter ports.AuditFilter, alreadyWritten int) ([]domain.AuditEvent, error) {
	batch := filter
	batch.Offset = filter.Offset + alreadyWritten
	batch.Limit = auditExportBatchSize
	if filter.Limit > 0 {
		remaining := filter.Limit - alreadyWritten
		if remaining <= 0 {
			return nil, nil
		}
		if remaining < batch.Limit {
			batch.Limit = remaining
		}
	}
	return h.Audit.List(r.Context(), batch)
}

// streamAuditBatches walks the audit_events table starting from the
// pre-fetched first batch, calling emit for each row and flush after
// each batch. Terminates when a batch returns fewer rows than requested
// (end of data) or when filter.Limit has been fully consumed.
func streamAuditBatches(
	r *http.Request,
	filter ports.AuditFilter,
	first []domain.AuditEvent,
	emit func(*domain.AuditEvent) error,
	flush func(),
	fetch func(*http.Request, ports.AuditFilter, int) ([]domain.AuditEvent, error),
) error {
	written := 0
	batch := first
	for {
		for i := range batch {
			if err := emit(&batch[i]); err != nil {
				return err
			}
			written++
		}
		flush()
		if len(batch) < auditExportBatchSize {
			return nil
		}
		if filter.Limit > 0 && written >= filter.Limit {
			return nil
		}
		next, err := fetch(r, filter, written)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			return nil
		}
		batch = next
	}
}

// auditEventCSVRow flattens an AuditEvent into the ordered string slice
// expected by encoding/csv. Column order matches auditExportCSVHeader.
func auditEventCSVRow(e *domain.AuditEvent) []string {
	return []string{
		e.ID,
		e.TS.UTC().Format(time.RFC3339),
		e.Actor,
		e.Method,
		e.Path,
		e.Route,
		strconv.Itoa(e.Status),
		e.ResourceType,
		e.ResourceID,
		e.BodyHash,
		e.ErrorDetail,
	}
}

// auditExportFilename derives the Content-Disposition filename from the
// filter's time bounds. When both since and until are set the range is
// baked into the name; otherwise a wall-clock timestamp is used so two
// exports run seconds apart do not collide.
func auditExportFilename(f ports.AuditFilter, ext string) string {
	const stamp = "20060102T150405Z"
	if !f.Since.IsZero() && !f.Until.IsZero() {
		return fmt.Sprintf("audit-%s-%s.%s",
			f.Since.UTC().Format(stamp), f.Until.UTC().Format(stamp), ext)
	}
	return fmt.Sprintf("audit-full-%s.%s", time.Now().UTC().Format(stamp), ext)
}
