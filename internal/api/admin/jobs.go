package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// JobsHandler serves /admin/jobs endpoints.
//
// Unlike the /v1/jobs handler, this surface is operator-facing: it lists
// jobs across every api_key_id / account_id and echoes internal accounting
// fields (pre_balance_h, actual_credits_h, charged_credits_h) that the
// public path deliberately hides. Callers reach it via the admin bearer
// token — no per-key scoping.
type JobsHandler struct {
	Jobs ports.JobStore
}

// NewJobsHandler wires a JobsHandler over the given store.
func NewJobsHandler(j ports.JobStore) *JobsHandler {
	return &JobsHandler{Jobs: j}
}

// Register mounts the routes under /admin/jobs.
func (h *JobsHandler) Register(r chi.Router) {
	r.Get("/jobs", h.List)
	r.Get("/jobs/{id}", h.Get)
}

// defaultAdminJobsLimit / maxAdminJobsLimit mirror the store-side caps so
// the value echoed in the response is always the effective one.
const (
	defaultAdminJobsLimit = 100
	maxAdminJobsLimit     = 500
)

// List serves GET /admin/jobs. Query params (all optional):
//
//	status              domain.JobStatus value (e.g. "completed")
//	account_id          filter by pool account
//	api_key_id          filter by standalone API key
//	group_id            filter by pool group
//	model_alias         filter by model alias
//	since / until       RFC3339 timestamps (falls back to unix seconds)
//	limit               page size; defaults to 100, capped at 500
//	offset              zero-based page offset
func (h *JobsHandler) List(w http.ResponseWriter, r *http.Request) {
	filter, ok := parseAdminJobsQuery(w, r)
	if !ok {
		return
	}
	rows, err := h.Jobs.ListAll(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, adminJobView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   data,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// Get serves GET /admin/jobs/{id}. Returns the full admin view, including
// internal accounting fields. 404 when the job does not exist.
func (h *JobsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid_id", "job id is required")
		return
	}
	j, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrJobNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "job not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, adminJobView(j))
}

// parseAdminJobsQuery pulls filter / paging params off r. On invalid input
// it writes a 400 and returns ok=false so the caller can bail. Limit is
// normalised here (default 100, cap 500) so the response echoes the
// effective value the store will apply.
func parseAdminJobsQuery(w http.ResponseWriter, r *http.Request) (ports.JobFilter, bool) {
	q := r.URL.Query()
	out := ports.JobFilter{
		Status:     domain.JobStatus(q.Get("status")),
		AccountID:  q.Get("account_id"),
		APIKeyID:   q.Get("api_key_id"),
		GroupID:    q.Get("group_id"),
		ModelAlias: q.Get("model_alias"),
		Limit:      defaultAdminJobsLimit,
	}
	if raw := q.Get("since"); raw != "" {
		t, err := parseAdminJobsTimeParam(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "since: "+err.Error())
			return ports.JobFilter{}, false
		}
		out.Since = t
	}
	if raw := q.Get("until"); raw != "" {
		t, err := parseAdminJobsTimeParam(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "until: "+err.Error())
			return ports.JobFilter{}, false
		}
		out.Until = t
	}
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_query", "limit must be a non-negative integer")
			return ports.JobFilter{}, false
		}
		if v == 0 {
			v = defaultAdminJobsLimit
		}
		if v > maxAdminJobsLimit {
			v = maxAdminJobsLimit
		}
		out.Limit = v
	}
	if raw := q.Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_query", "offset must be a non-negative integer")
			return ports.JobFilter{}, false
		}
		out.Offset = v
	}
	return out, true
}

// parseAdminJobsTimeParam accepts an RFC3339 string first, then falls back
// to a unix-seconds integer. Same behaviour as the /admin/usage and /v1/jobs
// endpoints so operators building tooling see one time-parse rule.
func parseAdminJobsTimeParam(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Time{}, errInvalidAdminJobsTime
}

// errInvalidAdminJobsTime is a sentinel for the parseAdminJobsTimeParam
// error path so the 400 message stays short and consistent across
// since/until.
var errInvalidAdminJobsTime = &adminJobsParseError{"expected RFC3339 or unix seconds"}

type adminJobsParseError struct{ msg string }

func (e *adminJobsParseError) Error() string { return e.msg }

// adminJobView renders a domain.Job for operator consumption. In contrast
// to v1.jobView, this view intentionally keeps internal fields that the
// public /v1 surface hides so operators can audit accounting end-to-end:
//
//   - account_id / group_id / api_key_id / cpa_partner_id : pool + caller
//     identity — required to debug routing and per-key attribution.
//   - pre_balance_h : the subscription_balance snapshot captured before
//     the upstream job was created; needed to reconcile drift between the
//     sync proxy path and the async pollworker.
//   - actual_credits_h / charged_credits_h : the two metering columns. The
//     first is what the account was actually billed; the second is what
//     the caller was billed (after markup). Operators must see both.
//
// Timestamps go out as RFC3339 (not unix seconds like v1.jobView) so this
// surface matches /admin/usage — operators consuming both endpoints see one
// time format across the admin surface.
func adminJobView(j *domain.Job) map[string]any {
	v := map[string]any{
		"id":                j.ID,
		"api_key_id":        j.APIKeyID,
		"cpa_partner_id":    j.CPAPartnerID,
		"group_id":          j.GroupID,
		"account_id":        j.AccountID,
		"model_alias":       j.ModelAlias,
		"jst":               j.JST,
		"endpoint":          j.Endpoint,
		"status":            string(j.Status),
		"upstream_job_id":   j.UpstreamJobID,
		"upstream_cost":     j.UpstreamCost,
		"result_url":        j.ResultURL,
		"latency_ms":        j.LatencyMS,
		"poll_count":        j.PollCount,
		"pre_balance_h":     j.PreBalanceH,
		"actual_credits_h":  j.ActualCreditsHundredths,
		"charged_credits_h": j.ChargedCreditsHundredths,
		"refunded":          j.Refunded,
		"request_ts":        j.RequestTS.UTC().Format(time.RFC3339),
	}
	if !j.FinishedAt.IsZero() {
		v["finished_at"] = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	if j.ErrorType != "" {
		v["error"] = map[string]any{
			"type":    string(j.ErrorType),
			"message": j.ErrorDetail,
		}
	}
	if j.CallbackURL != "" {
		v["callback_url"] = j.CallbackURL
	}
	return v
}
