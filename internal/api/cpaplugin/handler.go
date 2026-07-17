// Package cpaplugin serves the /internal/* HTTP surface used by an upstream
// CPA platform to drive higgsgo in "Mode B" (cpa_plugin). Every route is
// gated by the deploy-wide bearer token (server.internal_bearer) mounted by
// the parent api package; this package only implements the handlers.
//
// The CPA partner concept is represented on top of the existing api_keys
// table without a schema change: keys created via /internal/register carry
// the CPA partner id in the created_by column with the prefix "cpa:".
// ListByCPAPartner-style operations therefore walk APIKeyStore.List and
// filter by that prefix on the Go side. When the pool grows large enough
// to make the scan expensive a dedicated column + index can be added
// without touching this package.
package cpaplugin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// cpaPartnerPrefix is stored in api_keys.created_by so keys minted via
// /internal/register can be located later without a schema change.
const cpaPartnerPrefix = "cpa:"

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

// listKeysForPartner walks APIKeyStore.List and returns rows whose
// created_by encodes the given partner id. Empty partnerID returns an
// empty slice so a misconfigured caller cannot dump every row.
func (h *Handler) listKeysForPartner(ctx context.Context, partnerID string) ([]domain.APIKey, error) {
	if partnerID == "" {
		return nil, nil
	}
	rows, err := h.APIKeys.List(ctx)
	if err != nil {
		return nil, err
	}
	tag := cpaPartnerPrefix + partnerID
	out := make([]domain.APIKey, 0, len(rows))
	for i := range rows {
		if rows[i].CreatedBy == tag {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

// partnerIDFromKey pulls the partner id back out of an api_keys row that
// was minted via /internal/register. Returns "" when the row is not a CPA
// key so callers can skip non-CPA rows cleanly.
func partnerIDFromKey(k *domain.APIKey) string {
	if k == nil {
		return ""
	}
	if !strings.HasPrefix(k.CreatedBy, cpaPartnerPrefix) {
		return ""
	}
	return strings.TrimPrefix(k.CreatedBy, cpaPartnerPrefix)
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
