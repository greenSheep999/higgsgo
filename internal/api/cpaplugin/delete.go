package cpaplugin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// HandleDelete soft-deletes a CPA partner by revoking every api_keys row
// that belongs to it. The rows themselves are preserved (revoke sets
// status='revoked') so historical usage_events keep resolving.
//
// Body is ignored — the partner id lives in the URL.
func (h *Handler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	partnerID := chi.URLParam(r, "partner_id")
	if partnerID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "partner_id is required")
		return
	}
	keys, err := h.listKeysForPartner(r.Context(), partnerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if len(keys) == 0 {
		writeErr(w, http.StatusNotFound, "partner_not_registered", "no api keys for partner_id")
		return
	}
	var disabled int
	var firstErr error
	for i := range keys {
		if keys[i].Status != "active" {
			continue
		}
		if err := h.APIKeys.Revoke(r.Context(), keys[i].ID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		disabled++
	}
	if firstErr != nil && disabled == 0 {
		writeErr(w, http.StatusInternalServerError, "internal", firstErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"partner_id": partnerID,
		"disabled":   disabled,
		"total_keys": len(keys),
	})
}
