package v1

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// videoRequest is the OpenAI-shaped request body we accept.
// Any extra keys are forwarded to higgsfield's params.
type videoRequest struct {
	Model    string         `json:"model"`
	Prompt   string         `json:"prompt"`
	ImageURL string         `json:"image_url,omitempty"`
	MediaID  string         `json:"media_id,omitempty"`
	Async    bool           `json:"async,omitempty"`
	Extra    map[string]any `json:"-"` // populated by unmarshal below
}

// HandleVideoGeneration serves POST /v1/videos/generations.
//
// Body:
//
//	{ "model": "seedance-2-0-mini", "prompt": "a red apple", ... }
//
// Returns a GenerationResponse (see core/proxy). Sync by default; set
// "async": true to return immediately with a queued job id.
func (h *Handler) HandleVideoGeneration(w http.ResponseWriter, r *http.Request) {
	raw, err := readAll(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var vr videoRequest
	if err := json.Unmarshal(raw, &vr); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	// Also pull unknown keys into vr.Extra for forwarding into params.
	var extraMap map[string]any
	_ = json.Unmarshal(raw, &extraMap)
	knownKeys := map[string]bool{"model": true, "prompt": true, "image_url": true, "media_id": true, "async": true}
	extra := make(map[string]any)
	for k, v := range extraMap {
		if !knownKeys[k] {
			extra[k] = v
		}
	}

	if vr.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "model is required")
		return
	}

	userParams := make(map[string]any)
	if vr.Prompt != "" {
		userParams["prompt"] = vr.Prompt
	}
	for k, v := range extra {
		userParams[k] = v
	}

	// Note whether the client explicitly set async. When absent we let the
	// service decide (see Service.AsyncByDefault).
	_, syncRequested := extraMap["async"]
	greq := proxy.GenerationRequest{
		Model:         vr.Model,
		UserParams:    userParams,
		Async:         vr.Async,
		SyncRequested: syncRequested,
	}
	if vr.MediaID != "" {
		greq.Media = &proxy.MediaInput{PreUploadedID: vr.MediaID, Type: "image", URL: vr.ImageURL}
	}

	resp, err := h.Service.Generate(r.Context(), greq)
	if err != nil {
		writeGenerationError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// writeGenerationError maps domain errors to HTTP responses.
func writeGenerationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrModelNotFound):
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
	case errors.Is(err, domain.ErrNoEligibleAccount):
		writeError(w, http.StatusServiceUnavailable, "no_account_available", err.Error())
	case errors.Is(err, domain.ErrUpstreamForbidden):
		writeError(w, http.StatusPaymentRequired, "plan_gate", err.Error())
	case errors.Is(err, domain.ErrUpstreamRateLimit):
		writeError(w, http.StatusTooManyRequests, "rate_limit", err.Error())
	case errors.Is(err, domain.ErrUpstreamBadBody):
		writeError(w, http.StatusBadRequest, "body_error", err.Error())
	case errors.Is(err, domain.ErrUpstreamTimeout):
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", err.Error())
	case errors.Is(err, domain.ErrUpstreamUnauthorized):
		writeError(w, http.StatusUnauthorized, "upstream_auth", err.Error())
	case errors.Is(err, domain.ErrUpstreamServerError):
		writeError(w, http.StatusBadGateway, "upstream_5xx", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
