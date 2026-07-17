package cpaplugin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// HandleBalance returns aggregate usage and quota for every api_keys row
// belonging to the given CPA partner. Response shape:
//
//	{
//	  "partner_id":    "cpa_xyz",
//	  "total_used_h":  1234,      // sum of monthly_used across keys
//	  "total_limit_h": 100000,    // sum of monthly_quota; 0 => unlimited
//	  "keys":          [{id, name, status, monthly_used_h, monthly_limit_h}, ...]
//	}
//
// When any single key has monthly_quota == 0 the aggregate total_limit_h
// is reported as 0 to signal "unlimited on at least one key" — the
// upstream CPA platform should treat that as "no hard cap".
func (h *Handler) HandleBalance(w http.ResponseWriter, r *http.Request) {
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

	view := make([]map[string]any, 0, len(keys))
	var (
		totalUsed  int64
		totalLimit int64
		hasUnlim   bool
	)
	for i := range keys {
		k := &keys[i]
		view = append(view, map[string]any{
			"id":              k.ID,
			"name":            k.Name,
			"status":          k.Status,
			"monthly_used_h":  k.MonthlyUsed,
			"monthly_limit_h": k.MonthlyQuota,
			"markup_pct":      k.MarkupPct,
		})
		totalUsed += k.MonthlyUsed
		if k.MonthlyQuota == 0 {
			hasUnlim = true
		} else {
			totalLimit += k.MonthlyQuota
		}
	}
	if hasUnlim {
		totalLimit = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"partner_id":    partnerID,
		"total_used_h":  totalUsed,
		"total_limit_h": totalLimit,
		"key_count":     len(keys),
		"keys":          view,
	})
}
