package v1

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// defaultJobsListLimit / maxJobsListLimit mirror the store-side caps so the
// value we echo back in the response payload is always the effective one.
const (
	defaultJobsListLimit = 100
	maxJobsListLimit     = 500
)

// HandleJobFetch serves GET /v1/jobs/{id}.
//
// Returns the current job state from the JobStore. Callers poll this
// endpoint to observe async jobs reach terminal state.
//
// Response shape mirrors the create-time response so clients can reuse
// their parsing code.
func (h *Handler) HandleJobFetch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_id", "job id is required")
		return
	}
	if h.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_store_unavailable",
			"job persistence is not configured on this deployment")
		return
	}
	j, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrJobNotFound) {
			writeError(w, http.StatusNotFound, "job_not_found", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Callers that only asked for their own job (via the api key on the
	// request) should not be able to peek at rows belonging to another
	// key. The single-job endpoint historically did not enforce this, so
	// we do it here so /v1/jobs and /v1/jobs/{id} converge on the same
	// visibility rule.
	if key, ok := middleware.APIKeyFromContext(r.Context()); ok && key != nil && j.APIKeyID != "" && j.APIKeyID != key.ID {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
	resp := jobView(j)
	// The fetch endpoint historically returned a `data` array whenever a
	// result URL was known; keep that so existing clients keep parsing.
	if j.ResultURL != "" {
		resp["data"] = []map[string]string{{"url": j.ResultURL}}
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleJobsList serves GET /v1/jobs.
//
// Returns the caller's own jobs, newest first. Filters accepted via query
// string:
//
//	status              domain.JobStatus value (e.g. "completed")
//	since / until       RFC3339 timestamps (falls back to unix seconds)
//	limit               page size; defaults to 100, capped at 500
//	offset              zero-based page offset
//
// Auth comes from the /v1 middleware chain: the resolved APIKey lives on
// the request context via middleware.APIKeyFromContext. In CPA mode the
// public /v1 surface still runs through the same middleware so the same
// api_key_id is the natural scoping dimension here.
func (h *Handler) HandleJobsList(w http.ResponseWriter, r *http.Request) {
	if h.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_store_unavailable",
			"job persistence is not configured on this deployment")
		return
	}
	key, ok := middleware.APIKeyFromContext(r.Context())
	if !ok || key == nil {
		// The public /v1 mount always attaches APIKeyAuth in enforcing
		// mode, so reaching this branch means a wiring bug. Fail closed.
		writeError(w, http.StatusUnauthorized, "missing_api_key",
			"api key is required to list jobs")
		return
	}
	filter, ok := parseJobsListQuery(w, r)
	if !ok {
		return
	}
	rows, err := h.Jobs.ListByAPIKey(r.Context(), key.ID, filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		v := jobView(&rows[i])
		if rows[i].ResultURL != "" {
			v["data"] = []map[string]string{{"url": rows[i].ResultURL}}
		}
		data = append(data, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   data,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// parseJobsListQuery pulls filter / paging params off r. On invalid input
// it writes a 400 and returns ok=false so the caller can bail. Limit is
// normalised here (default 100, cap 500) so the response echoes the
// effective value the store will apply.
func parseJobsListQuery(w http.ResponseWriter, r *http.Request) (ports.JobFilter, bool) {
	q := r.URL.Query()
	out := ports.JobFilter{
		Status: domain.JobStatus(q.Get("status")),
		Limit:  defaultJobsListLimit,
	}
	if raw := q.Get("since"); raw != "" {
		t, err := parseJobsTimeParam(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", "since: "+err.Error())
			return ports.JobFilter{}, false
		}
		out.Since = t
	}
	if raw := q.Get("until"); raw != "" {
		t, err := parseJobsTimeParam(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_query", "until: "+err.Error())
			return ports.JobFilter{}, false
		}
		out.Until = t
	}
	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be a non-negative integer")
			return ports.JobFilter{}, false
		}
		if v == 0 {
			v = defaultJobsListLimit
		}
		if v > maxJobsListLimit {
			v = maxJobsListLimit
		}
		out.Limit = v
	}
	if raw := q.Get("offset"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid_query", "offset must be a non-negative integer")
			return ports.JobFilter{}, false
		}
		out.Offset = v
	}
	return out, true
}

// parseJobsTimeParam accepts an RFC3339 string first, then falls back to a
// unix-seconds integer. Same behaviour as the admin /usage endpoint so
// operators building tooling see one time-parse rule across the surface.
func parseJobsTimeParam(raw string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC(), nil
	}
	return time.Time{}, errInvalidJobsTime
}

// errInvalidJobsTime keeps the "since/until" 400 message stable across
// endpoints.
var errInvalidJobsTime = &jobsParseError{"expected RFC3339 or unix seconds"}

type jobsParseError struct{ msg string }

func (e *jobsParseError) Error() string { return e.msg }

// jobView renders a domain.Job as the OpenAI-shaped JSON view returned by
// both /v1/jobs/{id} and /v1/jobs. Deliberately omits internal accounting
// fields that must not leak to callers:
//
//   - pre_balance_h: caller's account subscription_balance snapshot used
//     by the async metering path; internal-only.
//   - account_id / group_id / actual_credits_h / charged_credits_h: pool
//     internals the caller has no business seeing.
//
// callback_url is echoed back because the caller supplied it themselves.
func jobView(j *domain.Job) map[string]any {
	v := map[string]any{
		"id":              j.ID,
		"object":          objectForOutputFromJob(j),
		"model":           j.ModelAlias,
		"status":          string(j.Status),
		"created_at":      j.RequestTS.Unix(),
		"upstream_job_id": j.UpstreamJobID,
		"cost":            j.UpstreamCost,
		"result_url":      j.ResultURL,
		"refunded":        j.Refunded,
		"latency_ms":      j.LatencyMS,
	}
	if !j.FinishedAt.IsZero() {
		v["finished_at"] = j.FinishedAt.Unix()
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

// objectForOutputFromJob derives the OpenAI-shaped "object" field from a
// job's model output type. We don't store the output type on the job row
// directly (yet); we look it up via the registry to keep the schema slim.
func objectForOutputFromJob(j *domain.Job) string {
	// The model registry may not be reachable here (no *Handler pointer
	// held for it in this package layout), so infer from JST heuristics as
	// a fallback. This is a cheap approximation that avoids extra plumbing.
	switch {
	case containsAny(j.JST, "video", "seedance", "kling", "veo3", "wan", "cinematic", "sora", "gemini_omni", "grok_video", "marketing", "hf_fnf", "happy_horse", "infinite_talk"):
		return "video"
	case containsAny(j.JST, "speech", "audio", "sonilo", "mirelo", "text2speech"):
		return "audio"
	default:
		return "image"
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if indexOf(s, n) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	// tiny substring search to avoid pulling in strings just for one call
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
