// Package admin exposes the /admin/* surface used by operators and the
// higgsgo-webui frontend. All endpoints require the bearer token configured
// in server.admin_bearer.
package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// KeysHandler serves /admin/keys endpoints. Groups + Usage are optional
// companions used only by the read-only detail endpoints (/keys/{id}/
// groups and /keys/{id}/stats); the mutating routes work without them.
type KeysHandler struct {
	Store  ports.APIKeyStore
	Groups ports.GroupStore
	Usage  ports.UsageEventStore
}

// NewKeysHandler wires a KeysHandler over the given store. Extra
// dependencies are set by the caller after construction so the
// existing single-arg call sites keep compiling.
func NewKeysHandler(store ports.APIKeyStore) *KeysHandler {
	return &KeysHandler{Store: store}
}

// Register mounts the CRUD routes under /admin/keys.
func (h *KeysHandler) Register(r chi.Router) {
	r.Get("/keys", h.List)
	r.Post("/keys", h.Create)
	r.Get("/keys/{id}", h.Get)
	r.Patch("/keys/{id}", h.Patch)
	r.Delete("/keys/{id}", h.Revoke)
	// Write ops beyond simple create/revoke. Each returns only the id
	// and the relevant field so the response never leaks key_hash.
	r.Post("/keys/{id}/rotate", h.Rotate)
	r.Post("/keys/{id}/pause", h.Pause)
	r.Post("/keys/{id}/resume", h.Resume)
	r.Post("/keys/{id}/reset_usage", h.ResetUsage)
	r.Post("/keys/{id}/playground_scope", h.UpdatePlaygroundScope)
	// Read-only companions used by the WebUI's Key detail page.
	r.Get("/keys/{id}/stats", h.Stats)
	r.Get("/keys/{id}/groups", h.ListGroups)
}

// List returns every api_keys row (never the plaintext).
func (h *KeysHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, apiKeyView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// createRequest is the body shape for POST /admin/keys.
type createRequest struct {
	Name            string  `json:"name"`
	CreatedBy       string  `json:"created_by,omitempty"`
	MonthlyQuota    int64   `json:"monthly_quota,omitempty"` // credits × 100, 0 = unlimited
	MarkupPct       float64 `json:"markup_pct,omitempty"`    // 1.0 = no markup
	PlaygroundScope string  `json:"playground_scope,omitempty"`
	Kind            string  `json:"kind,omitempty"` // default | project (defaults to project)
}

// Create issues a fresh key and returns the plaintext exactly once.
func (h *KeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req createRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.Name == "" {
		req.Name = "unnamed"
	}
	kind := parseAPIKeyKind(req.Kind)
	// Kind drives the plaintext prefix (sk-hg- vs sk-adm-) so the
	// key is visually classified from the moment it's shown.
	plaintext, hash, err := apikey.Generate(apikey.Kind(kind))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "gen_key", err.Error())
		return
	}
	last4 := apikey.Last4(plaintext)
	scope, ok := parsePlaygroundScope(req.PlaygroundScope)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_body",
			"playground_scope must be one of none|cheap|full")
		return
	}
	k := &domain.APIKey{
		ID:              idgen.NewID("key"),
		KeyHash:         hash,
		Name:            req.Name,
		CreatedBy:       req.CreatedBy,
		Status:          domain.APIKeyStatusActive,
		MonthlyQuota:    req.MonthlyQuota,
		MarkupPct:       req.MarkupPct,
		CreatedAt:       time.Now().UTC(),
		PlaygroundScope: scope,
		Kind:            kind,
		KeyLast4:        last4,
	}
	if err := h.Store.Create(r.Context(), k); err != nil {
		writeErr(w, http.StatusInternalServerError, "insert", err.Error())
		return
	}
	// Return the plaintext exactly once. The caller MUST store it.
	view := apiKeyView(k)
	view["plaintext_key"] = plaintext
	view["display_hint"] = "Store this key now — it will not be shown again."
	writeJSON(w, http.StatusCreated, view)
}

// Get returns one key by id.
func (h *KeysHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	k, err := h.Store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiKeyView(k))
}

// Revoke deactivates a key.
func (h *KeysHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Store.Revoke(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": domain.APIKeyStatusRevoked})
}

// Rotate mints a fresh plaintext for an existing key and returns it
// exactly once. All other columns (name, quota, markup, group bindings)
// are preserved so downstream routing / accounting keeps working across
// the rotation.
func (h *KeysHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	plaintext, err := h.Store.Rotate(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"key":          plaintext,
		"display_hint": "Store this key now — it will not be shown again.",
	})
}

// Pause suspends a key without soft-deleting it. Usage counters and group
// bindings are preserved so a resume flips the row back to the exact
// same state.
func (h *KeysHandler) Pause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Store.Pause(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		if errors.Is(err, domain.ErrAPIKeyRevoked) {
			writeErr(w, http.StatusConflict, "api_key_revoked", "revoked keys cannot be paused")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": domain.APIKeyStatusPaused})
}

// Resume flips a paused key back to active. Revoked keys are terminal
// and cannot be resumed — the store surfaces ErrAPIKeyRevoked in that
// case and we translate it to a 409.
func (h *KeysHandler) Resume(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Store.Resume(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		if errors.Is(err, domain.ErrAPIKeyRevoked) {
			writeErr(w, http.StatusConflict, "api_key_revoked", "revoked keys cannot be resumed")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": domain.APIKeyStatusActive})
}

// ResetUsage zeros the monthly_used counter. Called manually by an
// operator on refund / complaint flows or (eventually) automatically by
// a month-boundary ticker. Never touches monthly_quota so the caller
// keeps their configured cap.
func (h *KeysHandler) ResetUsage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Store.ResetMonthlyUsage(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "monthly_used": 0})
}

// apiKeyView is the public-safe representation of an APIKey (no plaintext).
func apiKeyView(k *domain.APIKey) map[string]any {
	scope := k.PlaygroundScope
	if scope == "" {
		scope = domain.PlaygroundScopeNone
	}
	kind := k.Kind
	if kind == "" {
		kind = domain.APIKeyKindProject
	}
	v := map[string]any{
		"id":               k.ID,
		"name":             k.Name,
		"created_by":       k.CreatedBy,
		"status":           k.Status,
		"monthly_quota":    k.MonthlyQuota,
		"monthly_used":     k.MonthlyUsed,
		"markup_pct":       k.MarkupPct,
		"created_at":       k.CreatedAt.UTC().Format(time.RFC3339),
		"playground_scope": string(scope),
		"kind":             string(kind),
		"key_last4":        k.KeyLast4,
	}
	if !k.LastUsedAt.IsZero() {
		v["last_used_at"] = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// parseAPIKeyKind validates + normalises the caller-provided kind on
// POST /admin/keys. Unknown / empty values fall back to "project" so
// a typo can't accidentally hand a caller the broader-access
// "default" tier.
func parseAPIKeyKind(raw string) domain.APIKeyKind {
	switch domain.APIKeyKind(raw) {
	case domain.APIKeyKindDefault:
		return domain.APIKeyKindDefault
	default:
		return domain.APIKeyKindProject
	}
}

// playgroundScopeRequest is the body shape for
// POST /admin/keys/{id}/playground_scope.
type playgroundScopeRequest struct {
	Scope string `json:"scope"`
}

// patchKeyRequest is the body shape for PATCH /admin/keys/{id}. All
// fields are pointers so callers can send partial updates. Playground
// scope + status transitions have their own endpoints — this path only
// covers labels + quota + markup so a typo can't accidentally revoke
// a key or open its scope.
type patchKeyRequest struct {
	Name         *string  `json:"name,omitempty"`
	MonthlyQuota *int64   `json:"monthly_quota,omitempty"`
	MarkupPct    *float64 `json:"markup_pct,omitempty"`
}

// Patch updates the label + budget + markup on an existing key.
// Returns 400 on invalid values, 404 when the key does not exist, and
// echoes the fresh apiKeyView on success.
func (h *KeysHandler) Patch(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req patchKeyRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.Name != nil && *req.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "name must not be empty")
		return
	}
	if req.MonthlyQuota != nil && *req.MonthlyQuota < 0 {
		writeErr(w, http.StatusBadRequest, "invalid_body", "monthly_quota must be >= 0")
		return
	}
	if req.MarkupPct != nil && *req.MarkupPct < 0 {
		writeErr(w, http.StatusBadRequest, "invalid_body", "markup_pct must be >= 0")
		return
	}
	err = h.Store.UpdateMeta(r.Context(), id, ports.APIKeyMetaPatch{
		Name:         req.Name,
		MonthlyQuota: req.MonthlyQuota,
		MarkupPct:    req.MarkupPct,
	})
	if err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	fresh, err := h.Store.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiKeyView(fresh))
}

// Stats returns a small usage rollup for a single key. Intended for the
// WebUI's Key detail page — the same data is also reachable via the
// generic /admin/usage/aggregate?group_by=... path, but shipping a
// dedicated shape keeps the front-end simpler and lets us evolve the
// summary without churning the aggregate schema.
//
// Query params:
//
//	since=30d    a Go duration parsed with time.ParseDuration; default 720h
//
// Returns 503 when Usage is not wired.
func (h *KeysHandler) Stats(w http.ResponseWriter, r *http.Request) {
	if h.Usage == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "usage store not configured")
		return
	}
	id := chi.URLParam(r, "id")
	// Confirm the key exists first so the caller gets a 404 instead of
	// an empty rollup when it's asking about a stale id.
	if _, err := h.Store.Get(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	window := 30 * 24 * time.Hour
	if raw := r.URL.Query().Get("since"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			window = d
		}
	}
	since := time.Now().Add(-window).UTC()

	rows, err := h.Usage.Aggregate(r.Context(), ports.UsageAggQuery{
		Since:   since,
		GroupBy: []string{"status"},
		Filters: ports.UsageQuery{APIKeyID: id},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var totalReq, completed, failed, refunded int64
	var totalCredits, chargedCredits int64
	for _, r := range rows {
		totalReq += r.RequestCount
		completed += r.CompletedCount
		failed += r.FailedCount
		refunded += r.RefundedCount
		totalCredits += r.TotalCreditsHundredths
		chargedCredits += r.ChargedCreditsHundredths
	}

	byModel, err := h.Usage.Aggregate(r.Context(), ports.UsageAggQuery{
		Since:   since,
		GroupBy: []string{"model_alias"},
		Filters: ports.UsageQuery{APIKeyID: id},
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	models := make([]map[string]any, 0, len(byModel))
	for _, r := range byModel {
		models = append(models, map[string]any{
			"model_alias":       r.Keys["model_alias"],
			"request_count":     r.RequestCount,
			"charged_credits_h": r.ChargedCreditsHundredths,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"since":             since.Format(time.RFC3339),
		"window":            window.String(),
		"request_count":     totalReq,
		"completed_count":   completed,
		"failed_count":      failed,
		"refunded_count":    refunded,
		"total_credits_h":   totalCredits,
		"charged_credits_h": chargedCredits,
		"by_model":          models,
	})
}

// ListGroups returns every group this key is bound to. Complements the
// existing /admin/groups/{id}/bindings (from-the-group direction);
// the WebUI needs from-the-key so it can render Key detail without
// walking every group.
func (h *KeysHandler) ListGroups(w http.ResponseWriter, r *http.Request) {
	if h.Groups == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "group store not configured")
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.Store.Get(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	groups, err := h.Groups.ListGroupsForAPIKey(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(groups))
	for i := range groups {
		data = append(data, groupView(&groups[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// UpdatePlaygroundScope handles POST /admin/keys/{id}/playground_scope.
// The body must carry a scope in {none, cheap, full}; anything else is
// rejected with a 400 so a typo cannot silently open access.
func (h *KeysHandler) UpdatePlaygroundScope(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req playgroundScopeRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	scope, ok := parsePlaygroundScope(req.Scope)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_body",
			"scope must be one of none|cheap|full")
		return
	}
	if err := h.Store.UpdatePlaygroundScope(r.Context(), id, scope); err != nil {
		if errors.Is(err, domain.ErrAPIKeyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "api key not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":               id,
		"playground_scope": string(scope),
	})
}

// parsePlaygroundScope normalises a raw scope string. An empty string
// resolves to PlaygroundScopeNone so a caller that omits the field on
// create keeps the migration-default locked-out behaviour. Unknown values
// return (_, false) so the handler can render a 400.
func parsePlaygroundScope(raw string) (domain.PlaygroundScope, bool) {
	switch raw {
	case "":
		return domain.PlaygroundScopeNone, true
	case string(domain.PlaygroundScopeNone),
		string(domain.PlaygroundScopeCheap),
		string(domain.PlaygroundScopeFull):
		return domain.PlaygroundScope(raw), true
	default:
		return "", false
	}
}

// --- shared helpers ------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"type": kind, "message": msg},
	})
}
