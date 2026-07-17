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

// KeysHandler serves /admin/keys endpoints.
type KeysHandler struct {
	Store ports.APIKeyStore
}

// NewKeysHandler wires a KeysHandler over the given store.
func NewKeysHandler(store ports.APIKeyStore) *KeysHandler {
	return &KeysHandler{Store: store}
}

// Register mounts the CRUD routes under /admin/keys.
func (h *KeysHandler) Register(r chi.Router) {
	r.Get("/keys", h.List)
	r.Post("/keys", h.Create)
	r.Get("/keys/{id}", h.Get)
	r.Delete("/keys/{id}", h.Revoke)
	// Write ops beyond simple create/revoke. Each returns only the id
	// and the relevant field so the response never leaks key_hash.
	r.Post("/keys/{id}/rotate", h.Rotate)
	r.Post("/keys/{id}/pause", h.Pause)
	r.Post("/keys/{id}/resume", h.Resume)
	r.Post("/keys/{id}/reset_usage", h.ResetUsage)
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
	Name         string  `json:"name"`
	CreatedBy    string  `json:"created_by,omitempty"`
	MonthlyQuota int64   `json:"monthly_quota,omitempty"` // credits × 100, 0 = unlimited
	MarkupPct    float64 `json:"markup_pct,omitempty"`    // 1.0 = no markup
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
	plaintext, hash, err := apikey.Generate()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "gen_key", err.Error())
		return
	}
	k := &domain.APIKey{
		ID:           idgen.NewID("key"),
		KeyHash:      hash,
		Name:         req.Name,
		CreatedBy:    req.CreatedBy,
		Status:       domain.APIKeyStatusActive,
		MonthlyQuota: req.MonthlyQuota,
		MarkupPct:    req.MarkupPct,
		CreatedAt:    time.Now().UTC(),
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
	v := map[string]any{
		"id":            k.ID,
		"name":          k.Name,
		"created_by":    k.CreatedBy,
		"status":        k.Status,
		"monthly_quota": k.MonthlyQuota,
		"monthly_used":  k.MonthlyUsed,
		"markup_pct":    k.MarkupPct,
		"created_at":    k.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !k.LastUsedAt.IsZero() {
		v["last_used_at"] = k.LastUsedAt.UTC().Format(time.RFC3339)
	}
	return v
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
