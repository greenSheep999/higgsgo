package cpaplugin

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// registerRequest is the body shape for POST /internal/register.
type registerRequest struct {
	PartnerID    string  `json:"partner_id"`
	Email        string  `json:"email,omitempty"`
	MarkupPct    float64 `json:"markup_pct,omitempty"`    // 1.0 = no markup
	MonthlyLimit int64   `json:"monthly_limit,omitempty"` // credits * 100, 0 = unlimited
	Name         string  `json:"name,omitempty"`
}

// HandleRegister mints a new higgsgo API key scoped to a CPA partner and
// returns the plaintext key exactly once. Body:
//
//	{
//	  "partner_id":    "cpa_xyz",
//	  "email":         "ops@example.com",
//	  "markup_pct":    1.2,
//	  "monthly_limit": 100000
//	}
//
// The partner_id is written to the dedicated api_keys.cpa_partner_id
// column (migration 004); the api_keys.created_by column is stamped
// with a fixed "cpa-plugin" tag so operators can tell at a glance which
// path minted the row.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req registerRequest
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_body", "empty body")
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.PartnerID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "partner_id is required")
		return
	}

	plaintext, hash, err := apikey.Generate()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "gen_key", err.Error())
		return
	}

	name := req.Name
	if name == "" {
		name = "cpa/" + req.PartnerID
	}
	k := &domain.APIKey{
		ID:           idgen.NewID("key"),
		KeyHash:      hash,
		Name:         name,
		CreatedBy:    cpaRegisterCreatedBy,
		CPAPartnerID: req.PartnerID,
		Status:       "active",
		MonthlyQuota: req.MonthlyLimit,
		MarkupPct:    req.MarkupPct,
		CreatedAt:    time.Now().UTC(),
	}
	if err := h.APIKeys.Create(r.Context(), k); err != nil {
		writeErr(w, http.StatusInternalServerError, "insert", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"api_key_id":     k.ID,
		"key":            plaintext,
		"cpa_partner_id": req.PartnerID,
		"markup_pct":     k.MarkupPct,
		"monthly_limit":  k.MonthlyQuota,
		"display_hint":   "Store this key now — it will not be shown again.",
	})
}
