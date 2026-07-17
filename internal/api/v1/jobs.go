package v1

import (
	"errors"
	"net/http"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/go-chi/chi/v5"
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
	resp := map[string]any{
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
		resp["finished_at"] = j.FinishedAt.Unix()
	}
	if j.ErrorType != "" {
		resp["error"] = map[string]any{
			"type":    string(j.ErrorType),
			"message": j.ErrorDetail,
		}
	}
	if j.ResultURL != "" {
		resp["data"] = []map[string]string{{"url": j.ResultURL}}
	}
	writeJSON(w, http.StatusOK, resp)
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
