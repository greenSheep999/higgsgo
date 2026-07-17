package v1

// Playground endpoints — /v1/playground/* — power the WebUI's interactive
// model tester. Callers authenticate with a regular API key whose
// playground_scope column (see migration 009) gates which of the 100+
// registered models they can invoke:
//
//   * scope=none   — every playground route returns 403 playground_disabled
//                    (blocked by middleware.PlaygroundGate)
//   * scope=cheap  — only models with EstCostHundredths <= domain.
//                    PlaygroundCheapCapHundredths (5 credits per generation)
//   * scope=full   — every registered model
//
// The middleware handles scope=none blanket denial; the per-endpoint code
// below performs the finer per-model scope check via
// domain.PlaygroundScope.AllowsModel so both /estimate and /execute agree
// on the cutoff.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// playgroundBlockedReasonCostTooHigh is the machine-friendly reason a
// scope-gated model view carries in the models listing response. Kept as
// a named constant so /models and /estimate stay in lockstep — a client
// filtering on this string can trust it will not silently change spelling.
const playgroundBlockedReasonCostTooHigh = "cost_too_high"

// HandlePlaygroundModels serves GET /v1/playground/models.
//
// Returns every registered model annotated with an `allowed` flag that
// reflects the caller's PlaygroundScope. The Unstable / Deprecated gates
// mirror /v1/models so the WebUI can toggle them via the same query
// params if it wants to.
func (h *Handler) HandlePlaygroundModels(w http.ResponseWriter, r *http.Request) {
	key, ok := middleware.APIKeyFromContext(r.Context())
	if !ok || key == nil {
		writeError(w, http.StatusForbidden, "playground_disabled",
			"api key is not permitted to use the playground")
		return
	}
	scope := effectivePlaygroundScope(key)
	q := r.URL.Query()
	regFilter := ports.ModelFilter{
		Output:            q.Get("output"),
		IncludeUnstable:   q.Get("include_unstable") == "1",
		IncludeDeprecated: q.Get("include_deprecated") == "1",
	}
	models := h.Registry.List(regFilter)
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		v := modelView(m)
		allowed := scope.AllowsModel(m.EstCostHundredths)
		v["allowed"] = allowed
		if !allowed && scope == domain.PlaygroundScopeCheap {
			v["blocked_reason"] = playgroundBlockedReasonCostTooHigh
		}
		data = append(data, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
		"scope":  string(scope),
		"total":  len(data),
	})
}

// playgroundEstimateRequest is the body shape for POST
// /v1/playground/estimate.
type playgroundEstimateRequest struct {
	Model  string         `json:"model"`
	Params map[string]any `json:"params,omitempty"`
}

// HandlePlaygroundEstimate serves POST /v1/playground/estimate.
//
// Resolves the requested model, checks the caller's PlaygroundScope
// permits it, and returns a cost preview along with the flags the WebUI
// needs to render an accurate "will charge X credits" banner. When the
// pool holds at least one unlim account and the model wants
// RequiresUnlim, we report will_charge=false so the WebUI can flag the
// call as free-from-user-credits.
func (h *Handler) HandlePlaygroundEstimate(w http.ResponseWriter, r *http.Request) {
	key, ok := middleware.APIKeyFromContext(r.Context())
	if !ok || key == nil {
		writeError(w, http.StatusForbidden, "playground_disabled",
			"api key is not permitted to use the playground")
		return
	}
	raw, err := readAll(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req playgroundEstimateRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if strings.TrimSpace(req.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "model is required")
		return
	}
	spec, err := h.Registry.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	scope := effectivePlaygroundScope(key)
	if !scope.AllowsModel(spec.EstCostHundredths) {
		writeError(w, http.StatusForbidden, "blocked_by_scope",
			"api key playground_scope does not permit this model")
		return
	}
	// A pool-side unlim override zeroes the caller-visible charge when
	// the model wants RequiresUnlim and the pool holds at least one
	// account with has_unlim=true. This mirrors the routing decision the
	// proxy service makes at execute time, so the estimate cannot lie by
	// promising free-then-charging.
	willCharge := true
	if spec.RequiresUnlim && h.hasUnlimAccountAvailable(r.Context()) {
		willCharge = false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model_alias":    spec.Alias,
		"output":         spec.Output,
		"cost_credits_h": spec.EstCostHundredths,
		"cost_credits":   float64(spec.EstCostHundredths) / 100.0,
		"needs_paid":     spec.RequiresPaid,
		"needs_ultra":    spec.RequiresUltra,
		"will_charge":    willCharge,
	})
}

// HandlePlaygroundExecute serves POST /v1/playground/execute.
//
// Body mirrors POST /v1/images/generations or /v1/videos/generations
// depending on the resolved spec.Output. The endpoint enforces the same
// per-model scope gate as /estimate before forwarding to the underlying
// generation handler.
//
// Sync is the default when the caller omits `async`. The underlying
// handler still honours an explicit async=true so power users can queue
// long-running video jobs from the WebUI without holding the HTTP
// request open.
func (h *Handler) HandlePlaygroundExecute(w http.ResponseWriter, r *http.Request) {
	key, ok := middleware.APIKeyFromContext(r.Context())
	if !ok || key == nil {
		writeError(w, http.StatusForbidden, "playground_disabled",
			"api key is not permitted to use the playground")
		return
	}
	raw, err := readAll(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	// Peek at the model field for the scope check; the underlying
	// image/video handler re-parses the full body from the request.
	var peek struct {
		Model string `json:"model"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &peek); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if strings.TrimSpace(peek.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "model is required")
		return
	}
	spec, err := h.Registry.Resolve(peek.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	scope := effectivePlaygroundScope(key)
	if !scope.AllowsModel(spec.EstCostHundredths) {
		writeError(w, http.StatusForbidden, "blocked_by_scope",
			"api key playground_scope does not permit this model")
		return
	}
	// Default async=false when the caller did not specify. Users who
	// explicitly send async=true keep their choice.
	forwarded := normalisePlaygroundBody(raw)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(forwarded))
	r2.ContentLength = int64(len(forwarded))
	if spec.Output == "image" {
		h.HandleImageGeneration(w, r2)
		return
	}
	h.HandleVideoGeneration(w, r2)
}

// effectivePlaygroundScope returns the scope of the given key, normalising
// the empty string to PlaygroundScopeNone so downstream code can compare
// against the enum without a nil-guard. Kept as a helper so both the
// models / estimate / execute paths stay in sync.
func effectivePlaygroundScope(k *domain.APIKey) domain.PlaygroundScope {
	if k == nil {
		return domain.PlaygroundScopeNone
	}
	switch k.PlaygroundScope {
	case domain.PlaygroundScopeCheap, domain.PlaygroundScopeFull:
		return k.PlaygroundScope
	default:
		return domain.PlaygroundScopeNone
	}
}

// hasUnlimAccountAvailable reports whether the pool currently holds an
// active account with has_unlim=true. Used by /estimate to decide whether
// a RequiresUnlim model will actually charge the caller's credits — with
// an unlim account in the pool the proxy will route through
// use_unlim=true and the caller pays nothing.
//
// Best-effort: any Accounts backend errors resolve to "no unlim available"
// so /estimate stays honest (worst case it over-reports will_charge).
func (h *Handler) hasUnlimAccountAvailable(ctx context.Context) bool {
	if h.Accounts == nil {
		return false
	}
	yes := true
	rows, err := h.Accounts.List(ctx, ports.AccountFilter{
		Status:   domain.StatusActive,
		HasUnlim: &yes,
	})
	if err != nil {
		return false
	}
	return len(rows) > 0
}

// normalisePlaygroundBody defaults async=false when the caller omits the
// field. If the caller already set async we leave the body untouched so
// their explicit choice wins.
func normalisePlaygroundBody(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if _, present := m["async"]; present {
		return raw
	}
	m["async"] = false
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
