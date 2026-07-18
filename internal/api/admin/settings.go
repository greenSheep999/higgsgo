package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/bearer"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// SettingsHandler serves /admin/settings/* endpoints — currently only
// the admin-bearer rotate flow. Kept separate from the other admin
// handlers because it owns a reference to the runtime-mutable
// bearer.Manager instead of a plain store.
type SettingsHandler struct {
	Bearer   *bearer.Manager
	Settings ports.SettingsStore
}

// NewSettingsHandler wires a SettingsHandler over the given manager
// and store. Both are required — a nil Manager would mean the /admin
// surface has no dynamic bearer to rotate; a nil store would mean the
// rotation is not persisted across restart.
func NewSettingsHandler(mgr *bearer.Manager, store ports.SettingsStore) *SettingsHandler {
	return &SettingsHandler{Bearer: mgr, Settings: store}
}

// Register mounts the settings routes on the given /admin router.
func (h *SettingsHandler) Register(r chi.Router) {
	r.Get("/settings/bearer", h.GetBearer)
	r.Post("/settings/bearer/rotate", h.RotateBearer)
}

// GetBearer returns metadata about the currently-active admin bearer.
// The plaintext value is never returned — even to a caller that just
// authenticated with it. The WebUI reads {source, last_4, updated_at}
// to render "Using DB · ····abcd" style badges.
func (h *SettingsHandler) GetBearer(w http.ResponseWriter, r *http.Request) {
	cur := h.Bearer.Current()
	src := string(h.Bearer.CurrentSource())
	var updated string
	if h.Bearer.CurrentSource() == bearer.SourceDB && h.Settings != nil {
		ts, err := h.Settings.UpdatedAt(r.Context(), bearer.SettingKey)
		if err == nil {
			updated = ts.UTC().Format(time.RFC3339)
		} else if !errors.Is(err, domain.ErrSettingNotFound) {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source":     src,
		"last_4":     bearer.Last4(cur),
		"updated_at": updated,
	})
}

// rotateBearerRequest is the body shape for POST
// /admin/settings/bearer/rotate. `current_bearer` is required so an
// attacker who steals a browser session cannot rotate the operator
// out of their own deploy; the check is constant-time via
// bearer.Manager.Accepts (matches current OR still-in-grace previous).
type rotateBearerRequest struct {
	// CurrentBearer must match either the current or a grace-window
	// previous bearer; otherwise the request 403s.
	CurrentBearer string `json:"current_bearer"`

	// NewBearer is the operator-supplied replacement. When empty the
	// server generates a fresh 32-byte hex bearer.
	NewBearer string `json:"new_bearer"`
}

// RotateBearer swaps the admin bearer atomically. On success the
// plaintext new bearer is returned in the response body — this is the
// only time the value leaves the server, so the WebUI stores it in
// localStorage immediately and displays it to the operator with a
// "copy now, we will not show it again" hint.
//
// The old bearer remains valid for bearer.GraceWindow (30s) so
// in-flight XHRs from the SPA do not 401 mid-flight.
func (h *SettingsHandler) RotateBearer(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req rotateBearerRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.CurrentBearer == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body",
			"current_bearer is required")
		return
	}
	if !h.Bearer.Accepts(req.CurrentBearer) {
		writeErr(w, http.StatusForbidden, "invalid_current_bearer",
			"current_bearer does not match the active admin bearer")
		return
	}

	newBearer := req.NewBearer
	if newBearer == "" {
		gen, err := bearer.Generate()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "gen_bearer", err.Error())
			return
		}
		newBearer = gen
	} else if err := bearer.ValidateBearer(newBearer); err != nil {
		// Surface the specific validation failure via error.type so the
		// WebUI can render a targeted message.
		switch {
		case errors.Is(err, bearer.ErrEmptyBearer):
			writeErr(w, http.StatusBadRequest, "invalid_new_bearer", err.Error())
		case errors.Is(err, bearer.ErrBearerTooShort):
			writeErr(w, http.StatusBadRequest, "bearer_too_short", err.Error())
		case errors.Is(err, bearer.ErrBearerWhitespace):
			writeErr(w, http.StatusBadRequest, "bearer_whitespace", err.Error())
		default:
			writeErr(w, http.StatusBadRequest, "invalid_new_bearer", err.Error())
		}
		return
	}

	if err := h.Bearer.Rotate(r.Context(), newBearer); err != nil {
		writeErr(w, http.StatusInternalServerError, "rotate_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"new_bearer": newBearer,
		"source":     string(bearer.SourceDB),
		"last_4":     bearer.Last4(newBearer),
		"display_hint": "Store this bearer now — it is only returned once. " +
			"The previous bearer remains valid for 30 seconds so in-flight " +
			"requests do not fail.",
	})
}
