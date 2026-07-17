package admin

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// ModelHealthHandler serves /admin/model-health endpoints. It surfaces the
// model_health rows written by the regression Ticker so operators can see
// which JSTs are currently healthy, failing, or stale (i.e. haven't been
// probed for a long time). The table is bounded by the model catalog size
// (~130 JSTs × recent history), so List has no pagination.
type ModelHealthHandler struct {
	Health ports.ModelHealthStore
}

// NewModelHealthHandler wires a ModelHealthHandler over the given store.
func NewModelHealthHandler(h ports.ModelHealthStore) *ModelHealthHandler {
	return &ModelHealthHandler{Health: h}
}

// Register mounts the routes under /admin/model-health.
func (h *ModelHealthHandler) Register(r chi.Router) {
	r.Get("/model-health", h.List)
	r.Get("/model-health/{jst}", h.Get)
}

// List serves GET /model-health. Query params:
//
//	verdict         restrict to rows whose verdict matches (ok|failed|
//	                timeout|pending|completed|...). Filtered Go-side
//	                since the table is small.
//	stale_before    RFC3339 timestamp; only rows with checked_at strictly
//	                earlier than this are returned. Useful for "which
//	                models haven't been probed since <T>?".
//
// Rows are returned newest first (ORDER BY checked_at DESC in the store).
func (h *ModelHealthHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	verdict := q.Get("verdict")
	var staleBefore time.Time
	if raw := q.Get("stale_before"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "stale_before: expected RFC3339")
			return
		}
		staleBefore = t.UTC()
	}

	rows, err := h.Health.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if verdict != "" && string(row.Verdict) != verdict {
			continue
		}
		if !staleBefore.IsZero() && !row.CheckedAt.Before(staleBefore) {
			continue
		}
		data = append(data, modelHealthView(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// Get serves GET /model-health/{jst}. Returns the latest probe for the
// given JST, or 404 when the JST has never been probed.
func (h *ModelHealthHandler) Get(w http.ResponseWriter, r *http.Request) {
	jst := chi.URLParam(r, "jst")
	row, err := h.Health.Latest(r.Context(), jst)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if row == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no health rows recorded for jst")
		return
	}
	writeJSON(w, http.StatusOK, modelHealthView(row))
}

// modelHealthView is the JSON representation of a ports.ModelHealthRow.
// All fields are exposed verbatim — /admin/model-health is an operator
// surface with no PII or credentials.
func modelHealthView(r *ports.ModelHealthRow) map[string]any {
	return map[string]any{
		"jst":           r.JST,
		"checked_at":    r.CheckedAt.UTC().Format(time.RFC3339),
		"verdict":       string(r.Verdict),
		"http_status":   r.HTTPStatus,
		"cost":          r.Cost,
		"poll_time_sec": r.PollTimeSec,
	}
}
