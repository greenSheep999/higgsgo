package cpaplugin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// HandleRefreshJWT force-invalidates the cached upstream JWT for every
// account currently reachable to higgsgo. This is a coarse implementation
// because the apikey -> account mapping is not yet modelled: the CPA
// partner id only pins which api_keys rows belong to the partner; those
// keys can currently draw from every account in the pool.
//
// The response reports how many accounts had their cache flushed so the
// caller can wire up alerting when the flush hits zero (which would
// indicate a broken pool wiring).
func (h *Handler) HandleRefreshJWT(w http.ResponseWriter, r *http.Request) {
	partnerID := chi.URLParam(r, "partner_id")
	if partnerID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "partner_id is required")
		return
	}
	// Confirm the partner actually exists before we touch shared caches.
	keys, err := h.listKeysForPartner(r.Context(), partnerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if len(keys) == 0 {
		writeErr(w, http.StatusNotFound, "partner_not_registered", "no api keys for partner_id")
		return
	}
	if h.Accounts == nil || h.JWT == nil {
		writeErr(w, http.StatusServiceUnavailable, "not_ready", "accounts or jwt not configured")
		return
	}
	accs, err := h.Accounts.List(r.Context(), ports.AccountFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var invalidated int
	for i := range accs {
		h.JWT.Invalidate(accs[i].ID)
		invalidated++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"partner_id":  partnerID,
		"invalidated": invalidated,
		"scope":       "pool-wide", // remove when apikey->account membership lands
	})
}
