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
		Status:       "active",
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
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "revoked"})
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
