package v1

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// videoRequest is the OpenAI-shaped request body we accept.
// Any extra keys are forwarded to higgsfield's params.
type videoRequest struct {
	Model       string `json:"model"`
	Prompt      string `json:"prompt"`
	ImageURL    string `json:"image_url,omitempty"`
	MediaID     string `json:"media_id,omitempty"`
	Async       bool   `json:"async,omitempty"`
	CallbackURL string `json:"callback_url,omitempty"`
	// GroupID, when set, restricts the pool pick to a specific account
	// group. When empty, the handler auto-resolves the group from the
	// caller's api key bindings via resolveGroup.
	GroupID string         `json:"group_id,omitempty"`
	Extra   map[string]any `json:"-"` // populated by unmarshal below
}

// HandleVideoGeneration serves both video-generation paths:
//
//	POST /v1/video/generations   (new-api / OneAPI compatible; preferred)
//	POST /v1/videos/generations  (higgsgo legacy alias)
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
	knownKeys := map[string]bool{"model": true, "prompt": true, "image_url": true, "media_id": true, "async": true, "callback_url": true, "group_id": true}
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

	// Resolve which group scopes the pool pick. When the caller sets
	// group_id we honour it; otherwise resolveGroup walks the direct
	// api_keys.group_id column first and falls back to the M:N binding
	// table via GroupStore.
	var (
		apiKey   *domain.APIKey
		apiKeyID string
	)
	if key, ok := middleware.APIKeyFromContext(r.Context()); ok {
		apiKey = key
		apiKeyID = key.ID
	}
	groupCandidates, herr := resolveGroup(r.Context(), h.Groups, h.Logger, apiKey, vr.GroupID)
	if herr != nil {
		writeError(w, herr.Status, herr.Kind, herr.Message)
		return
	}
	// Primary group for accounting is the first candidate; spillover
	// (P3-10) tries the rest in order when the primary hits a
	// group-scoped capacity error. Empty string in position 0 means
	// "global pool", which never spills over — there's nowhere to
	// spill to.
	primaryGroup := groupCandidates[0]

	greq := proxy.GenerationRequest{
		Model:           vr.Model,
		UserParams:      userParams,
		Async:           vr.Async,
		SyncRequested:   syncRequested,
		CallbackURL:     vr.CallbackURL,
		GroupID:         primaryGroup,
		GroupCandidates: groupCandidates,
		APIKeyID:        apiKeyID,
	}
	// Forward quota state so proxy.Service.enforceKeyGates can
	// reject over-limit requests pre-pick (ROADMAP P2-9). Nil-safe:
	// no key in context means both fields stay zero, and zero
	// quota is the historical "unlimited" default so the gate
	// no-ops.
	if apiKey != nil {
		greq.APIKeyMonthlyQuota = apiKey.MonthlyQuota
		greq.APIKeyMonthlyUsed = apiKey.MonthlyUsed
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
	case errors.Is(err, domain.ErrGroupConcurrencyMax):
		// Group-aggregate concurrency cap tripped — the group has
		// otherwise-eligible accounts, they're just fully loaded right
		// now. Retryable; 429 is the right signal (unlike 503, which
		// implies "pool is dry").
		writeError(w, http.StatusTooManyRequests, "pool_saturated", err.Error())
	case errors.Is(err, domain.ErrModelBlocked):
		// Group policy explicitly blocks this model alias. 403 is
		// correct: authenticated but not authorized for this alias.
		writeError(w, http.StatusForbidden, "model_blocked", err.Error())
	case errors.Is(err, domain.ErrModelNotAllowed):
		// Group has an allowlist and this alias is not on it. Same
		// 403 shape as blocked; distinct error type so admins can
		// tell "explicit deny" from "not in allowlist" in logs.
		writeError(w, http.StatusForbidden, "model_not_allowed", err.Error())
	case errors.Is(err, domain.ErrGroupQuotaExhausted):
		// Group's monthly_credit_budget is exhausted. 402 matches how
		// individual account balance exhaustion maps.
		writeError(w, http.StatusPaymentRequired, "group_budget_exhausted", err.Error())
	case errors.Is(err, domain.ErrAPIKeyQuotaExceed):
		// Per-key monthly limit tripped pre-pick (ROADMAP P2-9).
		// Distinct from the middleware's same-code post-hoc check —
		// this one prevents the pool slot from being consumed at all.
		// Same HTTP shape (402 quota_exhausted) so downstream callers
		// don't need to distinguish "you were rejected before the
		// pool touched you" from "you drained the last credit
		// after".
		writeError(w, http.StatusPaymentRequired, "quota_exhausted", err.Error())
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
