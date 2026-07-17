// Package cpaplugin serves the /internal/* HTTP surface used by an upstream
// CPA platform to drive higgsgo in "Mode B" (cpa_plugin). Every route is
// gated by the deploy-wide bearer token (server.internal_bearer) mounted by
// the parent api package; this package only implements the handlers.
//
// The CPA partner concept lives on the api_keys table as its own
// cpa_partner_id column (see migration 004). Keys minted via
// /internal/register set this column to the partner id; every subsequent
// /internal/* lookup goes through APIKeyStore.ListByCPAPartner which is
// indexed on that column. Standalone keys (minted via /admin/keys) leave
// cpa_partner_id empty and are therefore invisible to these routes.
package cpaplugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// cpaRegisterCreatedBy is the fixed created_by value stamped on every
// api_keys row minted by /internal/register. The partner id lives on
// cpa_partner_id now; created_by is left as a plain audit tag so
// operators can still tell at a glance where the key came from.
const cpaRegisterCreatedBy = "cpa-plugin"

// ProxyInvoker is the subset of *proxy.Service the cpaplugin package needs
// to satisfy the /internal/execute route. Defined locally so tests can pass
// a fake and so the concrete proxy package stays untouched.
type ProxyInvoker interface {
	Generate(ctx context.Context, req proxy.GenerationRequest) (*proxy.GenerationResponse, error)
}

// JWTMinter is the subset of *jwt.Minter cpaplugin needs. Only Invalidate
// is used (per-account JWT cache flush).
type JWTMinter interface {
	Invalidate(accountID string)
}

// Handler owns the /internal/* handlers. All dependencies are ports so
// tests can supply fakes.
type Handler struct {
	APIKeys  ports.APIKeyStore
	Accounts ports.AccountStore
	Jobs     ports.JobStore
	Proxy    ProxyInvoker
	JWT      JWTMinter
	Logger   *slog.Logger
}

// New builds a Handler. Logger is optional; callers that pass nil get a
// discarding logger wired up on first use.
func New(apiKeys ports.APIKeyStore, accounts ports.AccountStore, jobs ports.JobStore, invoker ProxyInvoker, minter JWTMinter, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		APIKeys:  apiKeys,
		Accounts: accounts,
		Jobs:     jobs,
		Proxy:    invoker,
		JWT:      minter,
		Logger:   logger,
	}
}

// Register mounts every /internal/* route on the given chi router. The
// parent api package is expected to wrap this group in BearerAuth already.
func (h *Handler) Register(r chi.Router) {
	r.Post("/internal/register", h.HandleRegister)
	r.Post("/internal/execute", h.HandleExecute)
	r.Get("/internal/balance/{partner_id}", h.HandleBalance)
	r.Post("/internal/refresh_jwt/{partner_id}", h.HandleRefreshJWT)
	r.Delete("/internal/{partner_id}", h.HandleDelete)
	r.Get("/internal/registrations/{id}", h.HandleRegistrations)
	r.Get("/internal/status", h.HandleStatus)
	// events_ws deferred: gorilla/websocket is not yet a project
	// dependency. See package doc.
}

// listKeysForPartner delegates to APIKeyStore.ListByCPAPartner which is
// indexed on api_keys.cpa_partner_id. Empty partnerID returns an empty
// slice so a misconfigured caller cannot dump every row (the store
// contract mirrors this, but we keep the guard here as documentation).
func (h *Handler) listKeysForPartner(ctx context.Context, partnerID string) ([]domain.APIKey, error) {
	if partnerID == "" {
		return nil, nil
	}
	return h.APIKeys.ListByCPAPartner(ctx, partnerID)
}

// --- shared JSON helpers -------------------------------------------------
//
// Duplicated (intentionally) from admin.writeJSON / admin.writeErr so this
// package has no cross-handler dependency. Both helpers are trivial.

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
