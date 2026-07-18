package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// UsageHandler serves /admin/usage endpoints. It reads back the metering rows
// written by the terminal-job Recorder so operators can inspect per-key,
// per-account, and per-model consumption without touching the DB directly.
type UsageHandler struct {
	Usage ports.UsageEventStore
}

// NewUsageHandler wires a UsageHandler over the given store.
func NewUsageHandler(u ports.UsageEventStore) *UsageHandler {
	return &UsageHandler{Usage: u}
}

// Register mounts the routes under /admin/usage.
func (h *UsageHandler) Register(r chi.Router) {
	r.Get("/usage", h.List)
	r.Get("/usage/aggregate", h.Aggregate)
}

// defaultUsageLimit is the page size applied when the caller omits ?limit=.
const defaultUsageLimit = 100

// maxUsageLimit caps the caller-provided ?limit= so a single request cannot
// pull every event out of the table.
const maxUsageLimit = 1000

// aggregateGroupByWhitelist lists the group_by dimensions the /aggregate
// endpoint accepts. Anything outside this set is dropped silently so callers
// cannot smuggle arbitrary column names into the store layer.
var aggregateGroupByWhitelist = map[string]struct{}{
	"api_key_id":     {},
	"cpa_partner_id": {},
	"account_id":     {},
	"group_id":       {},
	"model_alias":    {},
	"billing_hour":   {}, // synthetic strftime('%Y-%m-%dT%H:00:00Z', ts) bucket
	"billing_day":    {},
	"billing_month":  {},
	"media_type":     {},
	"status":         {},
}

// List serves GET /usage. Query params:
//
//	since / until      RFC3339 timestamps (falls back to unix seconds)
//	api_key_id         filter by standalone API key
//	cpa_partner_id     filter by CPA partner
//	account_id         filter by upstream account
//	group_id           filter by pool group
//	model_alias        filter by model alias
//	status             filter by JobStatus value
//	limit / offset     paging; limit defaults to 100 and caps at 1000
func (h *UsageHandler) List(w http.ResponseWriter, r *http.Request) {
	q, ok := parseUsageQuery(w, r)
	if !ok {
		return
	}
	rows, err := h.Usage.Query(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, usageEventView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   data,
		"limit":  q.Limit,
		"offset": q.Offset,
	})
}

// Aggregate serves GET /usage/aggregate. Accepts the same filter params as
// List plus a `group_by=` CSV parameter. Unknown dimensions are ignored.
func (h *UsageHandler) Aggregate(w http.ResponseWriter, r *http.Request) {
	filter, ok := parseUsageQuery(w, r)
	if !ok {
		return
	}
	// group_by is CSV; filter to the whitelist so the store never sees an
	// unexpected column name.
	var groupBy []string
	if raw := r.URL.Query().Get("group_by"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			col := strings.TrimSpace(part)
			if _, ok := aggregateGroupByWhitelist[col]; ok {
				groupBy = append(groupBy, col)
			}
		}
	}
	rows, err := h.Usage.Aggregate(r.Context(), ports.UsageAggQuery{
		Since:   filter.Since,
		Until:   filter.Until,
		GroupBy: groupBy,
		Filters: filter,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, usageAggView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// parseUsageQuery pulls the shared filter/paging params from the request. On
// invalid input it writes a 400 and returns ok=false so the caller can bail.
func parseUsageQuery(w http.ResponseWriter, r *http.Request) (ports.UsageQuery, bool) {
	q := r.URL.Query()
	out := ports.UsageQuery{
		APIKeyID:     q.Get("api_key_id"),
		CPAPartnerID: q.Get("cpa_partner_id"),
		AccountID:    q.Get("account_id"),
		GroupID:      q.Get("group_id"),
		ModelAlias:   q.Get("model_alias"),
		Status:       q.Get("status"),
		Limit:        defaultUsageLimit,
	}
	if raw := q.Get("since"); raw != "" {
		t, err := parseTimeParam(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "since: "+err.Error())
			return ports.UsageQuery{}, false
		}
		out.Since = t
	}
	if raw := q.Get("until"); raw != "" {
		t, err := parseTimeParam(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "until: "+err.Error())
			return ports.UsageQuery{}, false
		}
		out.Until = t
	}
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_query", "limit must be a non-negative integer")
			return ports.UsageQuery{}, false
		}
		if v == 0 {
			v = defaultUsageLimit
		}
		if v > maxUsageLimit {
			v = maxUsageLimit
		}
		out.Limit = v
	}
	if raw := q.Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_query", "offset must be a non-negative integer")
			return ports.UsageQuery{}, false
		}
		out.Offset = v
	}
	return out, true
}

// parseTimeParam accepts RFC3339 first, then falls back to unix seconds.
func parseTimeParam(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Time{}, errInvalidTime
}

// errInvalidTime is a sentinel for the parseTimeParam error path so the
// message stays short and consistent across since/until.
var errInvalidTime = &usageParseError{"expected RFC3339 or unix seconds"}

type usageParseError struct{ msg string }

func (e *usageParseError) Error() string { return e.msg }

// usageEventView is the JSON representation of a UsageEvent. Keys match the
// usage_events column names for parity with reports that read the DB directly.
func usageEventView(e *domain.UsageEvent) map[string]any {
	v := map[string]any{
		"id":                e.ID,
		"ts":                e.TS.UTC().Format(time.RFC3339),
		"api_key_id":        e.APIKeyID,
		"cpa_partner_id":    e.CPAPartnerID,
		"cpa_user_id":       e.CPAUserID,
		"group_id":          e.GroupID,
		"account_id":        e.AccountID,
		"model_alias":       e.ModelAlias,
		"jst":               e.JST,
		"media_type":        e.MediaType,
		"upstream_cost":     e.UpstreamCost,
		"actual_credits_h":  e.ActualCreditsHundredths,
		"charged_credits_h": e.ChargedCreditsHundredths,
		"markup_pct":        e.MarkupPct,
		"status":            string(e.Status),
		"latency_ms":        e.LatencyMS,
		"poll_count":        e.PollCount,
		"error_type":        string(e.ErrorType),
		"higgsgo_job_id":    e.HiggsgoJobID,
		"upstream_job_id":   e.UpstreamJobID,
		"result_url":        e.ResultURL,
		"billing_month":     e.BillingMonth,
		"billing_day":       e.BillingDay,
	}
	return v
}

// usageAggView renders a single aggregation row. The "keys" field carries the
// group-by dimension values so the caller can key their charts / tables.
func usageAggView(r *ports.UsageAggRow) map[string]any {
	keys := r.Keys
	if keys == nil {
		keys = map[string]string{}
	}
	return map[string]any{
		"keys":              keys,
		"request_count":     r.RequestCount,
		"completed_count":   r.CompletedCount,
		"failed_count":      r.FailedCount,
		"refunded_count":    r.RefundedCount,
		"total_credits_h":   r.TotalCreditsHundredths,
		"charged_credits_h": r.ChargedCreditsHundredths,
		"avg_latency_ms":    r.AvgLatencyMS,
	}
}
