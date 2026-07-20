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
	scope, ok := resolvePlaygroundScope(r)
	if !ok {
		writeError(w, http.StatusForbidden, "playground_disabled",
			"api key is not permitted to use the playground")
		return
	}
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
	// AsAPIKeyID, when set by an admin-bearer caller, asks the handler to
	// evaluate the estimate under the named API key's identity (its
	// playground_scope in particular). Ignored for sk-hg- key callers,
	// which always run as themselves. See resolveExecutionIdentity.
	AsAPIKeyID string `json:"as_api_key_id,omitempty"`
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
	// Resolve the effective identity (scope) after reading the body so an
	// admin-bearer caller's as_api_key_id can switch the scope used for the
	// per-model gate below. Estimate never forwards to a generation handler,
	// so the resolved *APIKey is not injected into the context here.
	scope, _, herr := h.resolveExecutionIdentity(r, req.AsAPIKeyID)
	if herr != nil {
		writeError(w, herr.Status, herr.Kind, herr.Message)
		return
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
// Body mirrors POST /v1/images/generations or /v1/video/generations
// depending on the resolved spec.Output. The endpoint enforces the same
// per-model scope gate as /estimate before forwarding to the underlying
// generation handler.
//
// Sync is the default when the caller omits `async`. The underlying
// handler still honours an explicit async=true so power users can queue
// long-running video jobs from the WebUI without holding the HTTP
// request open.
func (h *Handler) HandlePlaygroundExecute(w http.ResponseWriter, r *http.Request) {
	raw, err := readAll(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	// Peek at the model field for the scope check plus the optional
	// as_api_key_id used by admin-bearer callers to run under a specific
	// key's identity. The underlying image/video handler re-parses the
	// full body from the (rewritten) request.
	var peek struct {
		Model      string `json:"model"`
		AsAPIKeyID string `json:"as_api_key_id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &peek); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	// Resolve the effective identity. For an admin-bearer caller supplying
	// as_api_key_id this returns the impersonated key so we can inject it
	// into the forwarded context — that makes downstream usage, markup,
	// group routing and quota all accrue against that key.
	scope, asKey, herr := h.resolveExecutionIdentity(r, peek.AsAPIKeyID)
	if herr != nil {
		writeError(w, herr.Status, herr.Kind, herr.Message)
		return
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
	if !scope.AllowsModel(spec.EstCostHundredths) {
		writeError(w, http.StatusForbidden, "blocked_by_scope",
			"api key playground_scope does not permit this model")
		return
	}
	// Default async=false when the caller did not specify. Users who
	// explicitly send async=true keep their choice.
	forwarded := normalisePlaygroundBody(raw)
	ctx := r.Context()
	if asKey != nil {
		// Impersonation: forward as the named key so the generation
		// handlers read it via middleware.APIKeyFromContext.
		ctx = middleware.ContextWithAPIKey(ctx, asKey)
	}
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(forwarded))
	r2.ContentLength = int64(len(forwarded))
	if spec.Output == "image" {
		h.HandleImageGeneration(w, r2)
		return
	}
	h.HandleVideoGeneration(w, r2)
}

// resolvePlaygroundScope inspects the request context for either the admin
// bearer marker (set by BearerAuth / PlaygroundAuth on a matching deploy
// bearer) or a resolved APIKey (set by APIKeyAuth / PlaygroundAuth on a
// matching sk-hg- token) and returns the effective scope for the caller.
//
// The returned ok flag is false only when neither credential is present —
// which in production means the request slipped past auth (a wiring bug).
// The handlers still fail closed in that case to defend against a future
// mount regression. Admin bearer callers always resolve to
// PlaygroundScopeFull; API-key callers get their column value normalised
// (empty / unknown → none) so downstream comparisons stay total.
func resolvePlaygroundScope(r *http.Request) (domain.PlaygroundScope, bool) {
	ctx := r.Context()
	if middleware.IsAdminBearer(ctx) {
		return domain.PlaygroundScopeFull, true
	}
	k, ok := middleware.APIKeyFromContext(ctx)
	if !ok || k == nil {
		return domain.PlaygroundScopeNone, false
	}
	switch k.PlaygroundScope {
	case domain.PlaygroundScopeCheap, domain.PlaygroundScopeFull:
		return k.PlaygroundScope, true
	default:
		return domain.PlaygroundScopeNone, true
	}
}

// resolveExecutionIdentity determines the effective playground scope for a
// /v1/playground/{estimate,execute} request and, when the caller is an admin
// bearer impersonating an API key via as_api_key_id, returns that key so the
// caller can inject it into the forwarded request context.
//
// Behaviour by caller type:
//
//   - admin bearer + as_api_key_id: look the key up, validate it, and run
//     under its playground_scope. A missing/revoked/paused key yields an
//     invalid_as_api_key error; a scope=none key yields playground_disabled
//     (mirroring what a real call with that key would hit at the gate). The
//     returned *APIKey is non-nil so execute can impersonate it.
//   - admin bearer without as_api_key_id: full scope, nil key (unchanged
//     admin behaviour — the request keeps running as the admin identity).
//   - sk-hg- key caller: always runs as itself; as_api_key_id is ignored so
//     a key cannot impersonate another. The key already lives in the request
//     context (injected by APIKeyAuth), so the returned *APIKey is nil.
//
// The returned *APIKey is non-nil only in the admin-impersonation case; a nil
// key means "leave the context's existing identity in place".
func (h *Handler) resolveExecutionIdentity(r *http.Request, asAPIKeyID string) (domain.PlaygroundScope, *domain.APIKey, *httpError) {
	ctx := r.Context()
	asAPIKeyID = strings.TrimSpace(asAPIKeyID)

	if middleware.IsAdminBearer(ctx) {
		if asAPIKeyID == "" {
			// Unchanged admin behaviour: full scope, no impersonation.
			return domain.PlaygroundScopeFull, nil, nil
		}
		if h.APIKeys == nil {
			return domain.PlaygroundScopeNone, nil, &httpError{
				Status:  http.StatusInternalServerError,
				Kind:    "as_api_key_unavailable",
				Message: "server is not configured to resolve as_api_key_id",
			}
		}
		k, err := h.APIKeys.Get(ctx, asAPIKeyID)
		if err != nil || k == nil {
			return domain.PlaygroundScopeNone, nil, &httpError{
				Status:  http.StatusBadRequest,
				Kind:    "invalid_as_api_key",
				Message: "as_api_key_id does not match a known API key",
			}
		}
		if k.Status != domain.APIKeyStatusActive {
			return domain.PlaygroundScopeNone, nil, &httpError{
				Status:  http.StatusBadRequest,
				Kind:    "invalid_as_api_key",
				Message: "as_api_key_id is not an active API key",
			}
		}
		scope := k.PlaygroundScope
		if scope == "" {
			scope = domain.PlaygroundScopeNone
		}
		// Fail closed on scope=none and any unrecognised value, mirroring
		// what a real call with this key would hit at PlaygroundGate.
		if scope != domain.PlaygroundScopeCheap && scope != domain.PlaygroundScopeFull {
			return domain.PlaygroundScopeNone, nil, &httpError{
				Status:  http.StatusForbidden,
				Kind:    "playground_disabled",
				Message: "the selected api key is not permitted to use the playground",
			}
		}
		return scope, k, nil
	}

	// sk-hg- key caller: ignore as_api_key_id and run as self. The scope
	// comes from the context-resolved key exactly as before.
	scope, ok := resolvePlaygroundScope(r)
	if !ok {
		return domain.PlaygroundScopeNone, nil, &httpError{
			Status:  http.StatusForbidden,
			Kind:    "playground_disabled",
			Message: "api key is not permitted to use the playground",
		}
	}
	return scope, nil, nil
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
