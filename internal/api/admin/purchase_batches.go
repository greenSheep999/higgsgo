package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/greensheep999/higgsgo/internal/core/pricingfloor"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// PurchaseBatchView is the JSON wire shape for a purchase_batches row.
// USD amounts are surfaced in TWO forms: raw micros (integer,
// authoritative) plus a float `usd` field for direct human display
// in the UI. Same convention pricing_decisions uses.
type PurchaseBatchView struct {
	ID                          string  `json:"id"`
	PurchasedAt                 string  `json:"purchased_at"`
	SourceChannel               string  `json:"source_channel"`
	SourceSeller                string  `json:"source_seller"`
	PlanType                    string  `json:"plan_type"`
	AccountsCount               int     `json:"accounts_count"`
	CreditsPerAccountHundredths int64   `json:"credits_per_account_hundredths"`
	CreditsPerAccount           float64 `json:"credits_per_account"`
	TotalPaidMicros             int64   `json:"total_paid_micros"`
	TotalPaidUSD                float64 `json:"total_paid_usd"`
	PaidCurrency                string  `json:"paid_currency"`
	PaidAmountOriginalMicros    int64   `json:"paid_amount_original_micros"`
	ExchangeRateUsed            float64 `json:"exchange_rate_used"`
	PricingClass                string  `json:"pricing_class"`
	PromotionType               string  `json:"promotion_type"`
	Active                      bool    `json:"active"`
	LinkedAccountEmail          string  `json:"linked_account_email"`
	Rationale                   string  `json:"rationale"`
	CreatedAt                   string  `json:"created_at"`
	UpdatedAt                   string  `json:"updated_at"`
	// UnitCostMicros is the derived per-credit cost of THIS batch:
	// total_paid_micros / (credits_per_account × accounts_count).
	// Zero when credits_per_account_hundredths is 0 (unlim_1day).
	// Handy for the UI table so operators can eyeball outliers
	// without opening a calculator.
	UnitCostMicros int64 `json:"unit_cost_micros"`
}

// purchaseBatchRequest is the request body for POST/PUT. Kept
// intentionally close to the domain type but with USD floats
// accepted for convenience — the handler converts to micros before
// storing. Field-level omission → keep prior value on update
// (see UpdatePurchaseBatch merge semantics).
type purchaseBatchRequest struct {
	ID                          *string  `json:"id,omitempty"`
	PurchasedAt                 *string  `json:"purchased_at,omitempty"`
	SourceChannel               *string  `json:"source_channel,omitempty"`
	SourceSeller                *string  `json:"source_seller,omitempty"`
	PlanType                    *string  `json:"plan_type,omitempty"`
	AccountsCount               *int     `json:"accounts_count,omitempty"`
	CreditsPerAccount           *float64 `json:"credits_per_account,omitempty"`
	CreditsPerAccountHundredths *int64   `json:"credits_per_account_hundredths,omitempty"`
	TotalPaidUSD                *float64 `json:"total_paid_usd,omitempty"`
	TotalPaidMicros             *int64   `json:"total_paid_micros,omitempty"`
	PaidCurrency                *string  `json:"paid_currency,omitempty"`
	PaidAmountOriginalMicros    *int64   `json:"paid_amount_original_micros,omitempty"`
	ExchangeRateUsed            *float64 `json:"exchange_rate_used,omitempty"`
	PricingClass                *string  `json:"pricing_class,omitempty"`
	PromotionType               *string  `json:"promotion_type,omitempty"`
	Active                      *bool    `json:"active,omitempty"`
	LinkedAccountEmail          *string  `json:"linked_account_email,omitempty"`
	Rationale                   *string  `json:"rationale,omitempty"`
}

// ListPurchaseBatches serves GET /admin/pricing/purchase-batches.
// Returns every batch (active + retired, all pricing_classes) plus
// the current effective unit cost alongside the config fallback so
// the UI can render the "your effective floor is $X µ/cr because of
// these 9 eligible rows" summary in one round-trip.
func (h *ModelsHandler) ListPurchaseBatches(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable",
			"pricing persistence is not configured")
		return
	}
	batches, err := h.Pricing.ListPurchaseBatches(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	result := pricingfloor.EffectiveUnitCost(batches, h.FloorReferenceUnitCostMicros)
	views := make([]PurchaseBatchView, 0, len(batches))
	for i := range batches {
		views = append(views, toPurchaseBatchView(&batches[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": views,
		"summary": map[string]any{
			"effective_unit_cost_micros": result.UnitCostMicros,
			"total_paid_micros":          result.TotalPaidMicros,
			"total_credits":              result.TotalCredits,
			"eligible_batches":           result.EligibleBatches,
			"fallback_applied":           result.FallbackApplied,
			"config_fallback_micros":     h.FloorReferenceUnitCostMicros,
		},
	})
}

// CreatePurchaseBatch handles POST /admin/pricing/purchase-batches.
// Server generates the id if the request omits one. Every required
// field must be present; missing fields return 400 with the specific
// field name so the UI can highlight it.
func (h *ModelsHandler) CreatePurchaseBatch(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "pricing persistence is not configured")
		return
	}
	var req purchaseBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	batch, err := buildBatchFromRequest(&req, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if batch.ID == "" {
		batch.ID = idgen.NewID("pb")
	}
	if batch.CreatedAt.IsZero() {
		batch.CreatedAt = time.Now().UTC()
	}
	if err := h.Pricing.UpsertPurchaseBatch(r.Context(), batch); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toPurchaseBatchView(batch))
}

// UpdatePurchaseBatch handles PUT /admin/pricing/purchase-batches/{id}.
// Semantics: merge with existing row — fields absent from the request
// keep their prior value. This lets the UI send only the changed
// fields (partial update) without needing a full row.
func (h *ModelsHandler) UpdatePurchaseBatch(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "pricing persistence is not configured")
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.Pricing.GetPurchaseBatch(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if existing == nil {
		writeErr(w, http.StatusNotFound, "not_found", "purchase batch not found: "+id)
		return
	}
	var req purchaseBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	// Force the id from the URL onto the request so a malicious /
	// buggy body cannot rename the row via update.
	req.ID = &id
	batch, err := buildBatchFromRequest(&req, existing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := h.Pricing.UpsertPurchaseBatch(r.Context(), batch); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toPurchaseBatchView(batch))
}

// DeletePurchaseBatch handles DELETE /admin/pricing/purchase-batches/{id}.
// Idempotent: unknown id returns 204, not 404 — this matches how the
// UI's "delete then refresh" flow expects to behave, and matches the
// store contract in ports/storage.go.
func (h *ModelsHandler) DeletePurchaseBatch(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "pricing persistence is not configured")
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.Pricing.DeletePurchaseBatch(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// buildBatchFromRequest is the merge-and-validate step shared by
// CreatePurchaseBatch (existing=nil, all fields must be present) and
// UpdatePurchaseBatch (existing supplied, request fields are patches).
// Returns a fully-populated domain.PurchaseBatch or an error listing
// the first missing/invalid field.
func buildBatchFromRequest(req *purchaseBatchRequest, existing *domain.PurchaseBatch) (*domain.PurchaseBatch, error) {
	var b domain.PurchaseBatch
	if existing != nil {
		b = *existing
	}
	// Defaults for new rows.
	if existing == nil {
		b.Active = true
		b.PricingClass = "normal"
		b.PromotionType = "none"
		b.AccountsCount = 1
		b.PaidCurrency = "USD"
		b.ExchangeRateUsed = 1.0
	}
	if req.ID != nil && *req.ID != "" {
		b.ID = *req.ID
	}
	if req.PurchasedAt != nil {
		t, err := parseTimeField("purchased_at", *req.PurchasedAt)
		if err != nil {
			return nil, err
		}
		b.PurchasedAt = t
	} else if existing == nil {
		b.PurchasedAt = time.Now().UTC()
	}
	if req.SourceChannel != nil {
		b.SourceChannel = strings.TrimSpace(*req.SourceChannel)
	}
	if req.SourceSeller != nil {
		b.SourceSeller = strings.TrimSpace(*req.SourceSeller)
	}
	if req.PlanType != nil {
		b.PlanType = strings.TrimSpace(*req.PlanType)
	}
	if req.AccountsCount != nil {
		b.AccountsCount = *req.AccountsCount
	}
	// credits_per_account_hundredths wins over the float form when
	// both are set (integer is authoritative). Float is convenience
	// for the UI.
	if req.CreditsPerAccountHundredths != nil {
		b.CreditsPerAccountHundredths = *req.CreditsPerAccountHundredths
	} else if req.CreditsPerAccount != nil {
		b.CreditsPerAccountHundredths = int64(*req.CreditsPerAccount * 100)
	}
	if req.TotalPaidMicros != nil {
		b.TotalPaidMicros = *req.TotalPaidMicros
	} else if req.TotalPaidUSD != nil {
		b.TotalPaidMicros = int64(*req.TotalPaidUSD * 1_000_000)
	}
	if req.PaidCurrency != nil {
		b.PaidCurrency = strings.ToUpper(strings.TrimSpace(*req.PaidCurrency))
	}
	if req.PaidAmountOriginalMicros != nil {
		b.PaidAmountOriginalMicros = *req.PaidAmountOriginalMicros
	} else if existing == nil {
		// New row and no explicit original amount → mirror USD so
		// the audit column is at least populated.
		b.PaidAmountOriginalMicros = b.TotalPaidMicros
	}
	if req.ExchangeRateUsed != nil {
		b.ExchangeRateUsed = *req.ExchangeRateUsed
	}
	if req.PricingClass != nil {
		b.PricingClass = strings.ToLower(strings.TrimSpace(*req.PricingClass))
	}
	if req.PromotionType != nil {
		b.PromotionType = strings.ToLower(strings.TrimSpace(*req.PromotionType))
	}
	// Backfill legacy rows that skipped the migration hydration —
	// storage adapter also does this on write for belt-and-braces.
	if b.PromotionType == "" {
		b.PromotionType = "none"
	}
	if req.Active != nil {
		b.Active = *req.Active
	}
	if req.LinkedAccountEmail != nil {
		b.LinkedAccountEmail = strings.TrimSpace(*req.LinkedAccountEmail)
	}
	if req.Rationale != nil {
		b.Rationale = *req.Rationale
	}

	// Validation: mandatory fields (checked AFTER merge so an update
	// can leave them unset). ID is validated at the caller (URL
	// param wins for updates; server-generated for creates).
	if b.SourceChannel == "" {
		return nil, errors.New("source_channel is required")
	}
	if b.PlanType == "" {
		return nil, errors.New("plan_type is required")
	}
	if b.TotalPaidMicros <= 0 {
		return nil, errors.New("total_paid_usd (or total_paid_micros) must be > 0")
	}
	if b.AccountsCount <= 0 {
		return nil, errors.New("accounts_count must be > 0")
	}
	// credits_per_account_hundredths CAN be 0 (unlim_1day), so
	// don't enforce > 0 here. But if it's negative, that's a bug.
	if b.CreditsPerAccountHundredths < 0 {
		return nil, errors.New("credits_per_account must be >= 0")
	}
	validClasses := map[string]bool{"normal": true, "activity": true, "bug": true, "promo": true}
	if !validClasses[b.PricingClass] {
		return nil, fmt.Errorf("pricing_class must be one of: normal, activity, bug, promo (got %q)", b.PricingClass)
	}
	validPromos := map[string]bool{
		"none": true, "first_signup": true, "unlim_1day": true, "standard_credit_boost": true,
	}
	if !validPromos[b.PromotionType] {
		return nil, fmt.Errorf("promotion_type must be one of: none, first_signup, unlim_1day, standard_credit_boost (got %q)", b.PromotionType)
	}
	validPlans := map[string]bool{
		"starter": true, "pro": true, "plus": true, "ultra": true,
	}
	if !validPlans[b.PlanType] {
		return nil, fmt.Errorf("plan_type must be one of: starter, pro, plus, ultra (got %q)", b.PlanType)
	}
	return &b, nil
}

func parseTimeField(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err == nil {
		return t.UTC(), nil
	}
	// Accept the plain-date form the UI's <input type="date">
	// sends so an operator can pick a day without needing to
	// know RFC3339 syntax.
	if t2, err2 := time.Parse("2006-01-02", value); err2 == nil {
		return t2.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%s: expected RFC3339 or YYYY-MM-DD (got %q)", field, value)
}

func toPurchaseBatchView(b *domain.PurchaseBatch) PurchaseBatchView {
	view := PurchaseBatchView{
		ID:                          b.ID,
		PurchasedAt:                 b.PurchasedAt.UTC().Format(time.RFC3339),
		SourceChannel:               b.SourceChannel,
		SourceSeller:                b.SourceSeller,
		PlanType:                    b.PlanType,
		AccountsCount:               b.AccountsCount,
		CreditsPerAccountHundredths: b.CreditsPerAccountHundredths,
		CreditsPerAccount:           float64(b.CreditsPerAccountHundredths) / 100,
		TotalPaidMicros:             b.TotalPaidMicros,
		TotalPaidUSD:                float64(b.TotalPaidMicros) / 1_000_000,
		PaidCurrency:                b.PaidCurrency,
		PaidAmountOriginalMicros:    b.PaidAmountOriginalMicros,
		ExchangeRateUsed:            b.ExchangeRateUsed,
		PricingClass:                b.PricingClass,
		PromotionType:               b.PromotionType,
		Active:                      b.Active,
		LinkedAccountEmail:          b.LinkedAccountEmail,
		Rationale:                   b.Rationale,
		CreatedAt:                   b.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:                   b.UpdatedAt.UTC().Format(time.RFC3339),
	}
	// Derived unit cost — same math the calculator uses per-row but
	// for a single batch, so the UI table can flag outliers at a
	// glance without a JS-side computation.
	credits := b.CreditsPerAccountHundredths * int64(b.AccountsCount) / 100
	if credits > 0 {
		view.UnitCostMicros = b.TotalPaidMicros / credits
	}
	return view
}
