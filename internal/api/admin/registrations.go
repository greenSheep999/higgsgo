package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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
	// Bulk import: POST /admin/registrations/bulk with a body of
	// { lines: "email----password----client_id----refresh_token\n...",
	//   proxy_url: "socks5://..." }.
	// Response carries {enqueued, ids, skipped[]} so a partial-batch
	// UI can show per-line errors. See ROADMAP §5.4 P4-3d.
	r.Post("/registrations/bulk", h.BulkEnqueue)
	r.Get("/registrations/{id}", h.Get)
	r.Post("/registrations/{id}/retry", h.Retry)
}

// enqueueRequest is the body shape for POST /admin/registrations.
//
// Password + MailboxClientID + MailboxRefreshToken are required for
// the password flow (OAuthSource == ""). They come from the operator's
// bulk mailbox list (line format:
// email----password----oauth_client_id----refresh_token) — every row
// carries its own Microsoft Graph OAuth2 credentials because different
// mailboxes have different app registrations. See ROADMAP §5.4 P4-3d.
type enqueueRequest struct {
	Email               string `json:"email"`
	Password            string `json:"password,omitempty"`
	OAuthSource         string `json:"oauth_source,omitempty"`
	ProxyURL            string `json:"proxy_url,omitempty"`
	MailboxClientID     string `json:"mailbox_client_id,omitempty"`
	MailboxRefreshToken string `json:"mailbox_refresh_token,omitempty"`
}

// bulkEnqueueRequest accepts a mailbox list in the higgsfield-register
// format: `email----password----client_id----refresh_token`, one per
// line. Blank lines and lines beginning with `#` are ignored. Each
// valid row is turned into a pending registration; the response
// carries the parse count so the operator can spot missed lines.
type bulkEnqueueRequest struct {
	// Lines is the raw text pasted from the mailbox list file. The
	// handler parses it line-by-line rather than requiring the
	// operator to convert to JSON — matches the way the file exists
	// on disk in operator workflows.
	Lines    string `json:"lines"`
	ProxyURL string `json:"proxy_url,omitempty"` // applied to every row
}

// Enqueue queues a new registration attempt. Password + mailbox
// credentials are required for the password flow; OAuth flows leave
// them empty.
func (h *RegistrationsHandler) Enqueue(w http.ResponseWriter, r *http.Request) {
	if h.Registrar == nil {
		writeRegistrarDisabled(w)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8192))
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
	if req.OAuthSource == "" {
		// Password flow: everything the Node driver needs to run
		// signup + OTP must be present. Refuse rather than let
		// the flow fail 90 seconds later with an obscure driver
		// error the operator has to hunt down.
		if req.Password == "" {
			writeErr(w, http.StatusBadRequest, "invalid_body",
				"password is required for the password flow")
			return
		}
		if req.MailboxClientID == "" || req.MailboxRefreshToken == "" {
			writeErr(w, http.StatusBadRequest, "invalid_body",
				"mailbox_client_id and mailbox_refresh_token are required for the password flow (needed to fetch the OTP email via Microsoft Graph)")
			return
		}
	}
	id, err := h.Registrar.Enqueue(r.Context(), ports.RegistrationRequest{
		Email:               req.Email,
		Password:            req.Password,
		OAuthSource:         req.OAuthSource,
		ProxyURL:            req.ProxyURL,
		MailboxClientID:     req.MailboxClientID,
		MailboxRefreshToken: req.MailboxRefreshToken,
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

// BulkEnqueue parses a mailbox list and queues one registration per
// valid line. Response body carries:
//
//	{ enqueued: N, skipped: [ {line: 3, reason: "..."}, ... ], ids: [ ... ] }
//
// A single malformed line does NOT abort the whole batch — it's
// recorded under `skipped` and the rest of the file still gets
// enqueued. This matches the "paste 100 lines and hit go" operator
// workflow: partial-success is more useful than all-or-nothing.
func (h *RegistrationsHandler) BulkEnqueue(w http.ResponseWriter, r *http.Request) {
	if h.Registrar == nil {
		writeRegistrarDisabled(w)
		return
	}
	// 512 KiB caps the paste to something a browser can trivially
	// send; that's ~5000 mailbox lines at 100 chars average.
	raw, err := io.ReadAll(io.LimitReader(r.Body, 512*1024))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req bulkEnqueueRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	type skipped struct {
		Line   int    `json:"line"`
		Reason string `json:"reason"`
	}
	var (
		ids      []string
		skips    []skipped
		enqueued int
	)
	for i, rawLine := range strings.Split(req.Lines, "\n") {
		lineNum := i + 1
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// higgsfield-register uses `----` as a field separator so
		// URL-safe base64 refresh tokens (which contain `_` and `-`)
		// don't collide with the delimiter.
		parts := strings.Split(line, "----")
		if len(parts) < 4 {
			skips = append(skips, skipped{
				Line: lineNum,
				Reason: fmt.Sprintf("expected 4 fields separated by ----, got %d",
					len(parts)),
			})
			continue
		}
		email := strings.TrimSpace(parts[0])
		password := strings.TrimSpace(parts[1])
		clientID := strings.TrimSpace(parts[2])
		refreshToken := strings.TrimSpace(parts[3])
		if email == "" || password == "" || clientID == "" || refreshToken == "" {
			skips = append(skips, skipped{
				Line:   lineNum,
				Reason: "one of email / password / client_id / refresh_token is blank",
			})
			continue
		}
		id, err := h.Registrar.Enqueue(r.Context(), ports.RegistrationRequest{
			Email:               email,
			Password:            password,
			ProxyURL:            req.ProxyURL,
			MailboxClientID:     clientID,
			MailboxRefreshToken: refreshToken,
		})
		if err != nil {
			if errors.Is(err, domain.ErrRegistrarDisabled) {
				writeRegistrarDisabled(w)
				return
			}
			skips = append(skips, skipped{
				Line:   lineNum,
				Reason: err.Error(),
			})
			continue
		}
		ids = append(ids, id)
		enqueued++
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"enqueued": enqueued,
		"ids":      ids,
		"skipped":  skips,
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
