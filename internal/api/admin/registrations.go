package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// RegistrationsHandler exposes the /admin/registrations surface used
// by the WebUI's Plugin > Registrations tab. It is a thin translator
// over ports.Registrar; when the deploy is built without the
// "register" tag, the underlying Registrar is a stub that returns
// domain.ErrRegistrarDisabled on every call — every route here then
// answers 503 with a stable {error:{type:"registrar_disabled",…}}
// envelope so the SPA can render a clear opt-in hint.
//
// A separate read of the same rows is exposed on the /internal/*
// listener by cpaplugin.Handler.HandleRegistrations for the upstream
// CPA platform's async status polls. Both routes must point at the
// same ports.Registrar instance (wired in server.New) so operators
// and CPA partners see one consistent picture.
type RegistrationsHandler struct {
	Registrar ports.Registrar
}

// NewRegistrationsHandler wires the handler over the given Registrar.
// A nil Registrar is treated the same as a stub — every route answers
// 503 registrar_disabled — so the admin router can mount this
// unconditionally.
func NewRegistrationsHandler(r ports.Registrar) *RegistrationsHandler {
	return &RegistrationsHandler{Registrar: r}
}

// Register mounts the CRUD routes under /registrations. The parent
// admin router already carries BearerAuth.
func (h *RegistrationsHandler) Register(r chi.Router) {
	r.Get("/registrations", h.List)
	r.Post("/registrations", h.Enqueue)
	r.Get("/registrations/{id}", h.Get)
	r.Post("/registrations/{id}/retry", h.Retry)
}

// enqueueRequest is the body shape for POST /admin/registrations.
type enqueueRequest struct {
	Email       string `json:"email"`
	OAuthSource string `json:"oauth_source,omitempty"`
	ProxyURL    string `json:"proxy_url,omitempty"`
}

// Enqueue queues a new registration attempt.
func (h *RegistrationsHandler) Enqueue(w http.ResponseWriter, r *http.Request) {
	if h.Registrar == nil {
		writeRegistrarDisabled(w)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req enqueueRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.Email == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "email is required")
		return
	}
	id, err := h.Registrar.Enqueue(r.Context(), ports.RegistrationRequest{
		Email:       req.Email,
		OAuthSource: req.OAuthSource,
		ProxyURL:    req.ProxyURL,
	})
	if err != nil {
		if errors.Is(err, domain.ErrRegistrarDisabled) {
			writeRegistrarDisabled(w)
			return
		}
		writeErr(w, http.StatusInternalServerError, "enqueue", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":     id,
		"status": "pending",
	})
}

// List returns recent registrations, newest first.
func (h *RegistrationsHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.Registrar == nil {
		writeRegistrarDisabled(w)
		return
	}
	filter := ports.RegistrationFilter{
		Status: r.URL.Query().Get("status"),
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Offset = n
		}
	}
	rows, err := h.Registrar.List(r.Context(), filter)
	if err != nil {
		if errors.Is(err, domain.ErrRegistrarDisabled) {
			writeRegistrarDisabled(w)
			return
		}
		writeErr(w, http.StatusInternalServerError, "list", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, registrationView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":   data,
		"limit":  filter.Limit,
		"offset": filter.Offset,
	})
}

// Get returns one registration by id.
func (h *RegistrationsHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.Registrar == nil {
		writeRegistrarDisabled(w)
		return
	}
	id := chi.URLParam(r, "id")
	row, err := h.Registrar.GetStatus(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRegistrarDisabled):
			writeRegistrarDisabled(w)
		case errors.Is(err, domain.ErrRegistrationNotFound):
			writeErr(w, http.StatusNotFound, "not_found", "registration not found")
		default:
			writeErr(w, http.StatusInternalServerError, "get", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, registrationView(row))
}

// Retry re-queues a failed / stuck registration.
func (h *RegistrationsHandler) Retry(w http.ResponseWriter, r *http.Request) {
	if h.Registrar == nil {
		writeRegistrarDisabled(w)
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.Registrar.Retry(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, domain.ErrRegistrarDisabled):
			writeRegistrarDisabled(w)
		case errors.Is(err, domain.ErrRegistrationNotFound):
			writeErr(w, http.StatusNotFound, "not_found", "registration not found")
		default:
			writeErr(w, http.StatusInternalServerError, "retry", err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "pending"})
}

// registrationView is the JSON representation of a RegistrationRow.
// Zero timestamps are omitted so the SPA can distinguish "not
// finished yet" from "finished at Unix epoch".
func registrationView(row *ports.RegistrationRow) map[string]any {
	v := map[string]any{
		"id":           row.ID,
		"email":        row.Email,
		"oauth_source": row.OAuthSource,
		"proxy_url":    row.ProxyURL,
		"status":       row.Status,
		"attempts":     row.Attempts,
		"last_error":   row.LastError,
		"account_id":   row.AccountID,
	}
	if !row.CreatedAt.IsZero() {
		v["created_at"] = row.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !row.FinishedAt.IsZero() {
		v["finished_at"] = row.FinishedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// writeRegistrarDisabled emits the 503 envelope every disabled-state
// route returns. Kept as a helper so the shape is impossible to drift.
func writeRegistrarDisabled(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{
			"type":    "registrar_disabled",
			"message": "registrar disabled (build with `-tags register` and configure to enable)",
		},
	})
}
