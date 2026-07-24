package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// pricingDecisionWarning is one non-blocking signal attached to a
// successfully-written decision. Written into 201 responses as
// `warnings: [...]`. Contract §10 defines `retail_below_floor` as the
// currently-shipping code; `cost_rule_missing` is emitted when the
// floor cannot be computed (no matching model_cost_rule) so the
// operator sees why the check silently skipped.
type pricingDecisionWarning struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	FloorMicros       int64  `json:"floor_micros,omitempty"`
	RetailMicros      int64  `json:"retail_micros,omitempty"`
	ReferenceUnitCost int64  `json:"reference_unit_cost_micros,omitempty"`
	MarkupMultiplier  string `json:"markup_multiplier,omitempty"`
	VariantCredits    string `json:"variant_credits,omitempty"`
}

// pricingDecisionRequest is the wire body for POST /admin/models/{alias}/pricing-decisions.
// One request appends exactly one row to model_price_decisions; the previous
// row for the same variant is left in place so history can be inspected.
type pricingDecisionRequest struct {
	Currency        string `json:"currency"`
	Unit            string `json:"unit"`
	PriceMicros     int64  `json:"price_micros"`
	Resolution      string `json:"resolution"`
	DurationSeconds int    `json:"duration_seconds"`
	Mode            string `json:"mode"`
	Audio           string `json:"audio"`
	Rationale       string `json:"rationale"`
}

// pricingDecisionView is the response shape after a successful append. It
// mirrors the domain struct but converts the timestamp to RFC3339 so the
// WebUI can display it without extra parsing. Warnings carry non-blocking
// signals; the row is written regardless.
type pricingDecisionView struct {
	ID              string                   `json:"id"`
	ModelAlias      string                   `json:"model_alias"`
	Currency        string                   `json:"currency"`
	Unit            string                   `json:"unit"`
	PriceMicros     int64                    `json:"price_micros"`
	USD             float64                  `json:"usd"`
	Resolution      string                   `json:"resolution"`
	DurationSeconds int                      `json:"duration_seconds"`
	Mode            string                   `json:"mode"`
	Audio           string                   `json:"audio"`
	Rationale       string                   `json:"rationale,omitempty"`
	DecidedAt       string                   `json:"decided_at"`
	Warnings        []pricingDecisionWarning `json:"warnings,omitempty"`
}

// CreatePricingDecision appends an operator-approved sell price for one
// (alias, resolution, duration, mode, audio, unit) variant. Idempotent by
// design of the underlying table: writing twice with the same body creates
// two rows and the newest wins on read. That matches how promo/pricing
// audits usually want to see the reasoning trail rather than a silent
// overwrite.
func (h *ModelsHandler) CreatePricingDecision(w http.ResponseWriter, r *http.Request) {
	if h.Registry == nil || h.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "pricing_unavailable",
			"pricing store or model registry is not configured")
		return
	}
	alias := strings.TrimSpace(chi.URLParam(r, "alias"))
	if alias == "" {
		writeErr(w, http.StatusBadRequest, "invalid_alias", "alias is required")
		return
	}
	// Same 404 guard the pricing-matrix reader uses: refuse to record a
	// decision for a model that no longer exists in the catalog.
	if _, err := h.Registry.Resolve(alias); err != nil {
		if errors.Is(err, domain.ErrModelNotFound) {
			writeErr(w, http.StatusNotFound, "model_not_found",
				"alias not in the model catalog")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req pricingDecisionRequest
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_body", "request body is required")
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.Unit == "" {
		writeErr(w, http.StatusBadRequest, "invalid_unit",
			"unit is required (e.g. per_second or per_request)")
		return
	}
	if req.PriceMicros < 0 {
		writeErr(w, http.StatusBadRequest, "invalid_price",
			"price_micros must be non-negative")
		return
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}

	// Resolve alias → JST so the floor lookup can match rules against
	// the same key pricing_matrix.go uses. Errors already 404'd above;
	// re-resolve here to avoid re-reading the body path.
	spec, _ := h.Registry.Resolve(alias)

	stored, err := h.Pricing.RecordPriceDecision(r.Context(), domain.ModelPriceDecision{
		ModelAlias:      alias,
		Currency:        req.Currency,
		Unit:            req.Unit,
		PriceMicros:     req.PriceMicros,
		Resolution:      req.Resolution,
		DurationSeconds: req.DurationSeconds,
		Mode:            req.Mode,
		Audio:           req.Audio,
		Rationale:       strings.TrimSpace(req.Rationale),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Compute the soft retail_below_floor warning AFTER the write
	// succeeds. Contract §10: the row is durable either way; the
	// warning is a signal, not a gate. A rule-lookup failure emits
	// `cost_rule_missing` so the operator can see why the floor was
	// silently skipped rather than assume approval.
	warnings := h.retailFloorWarnings(r, spec, stored)

	writeJSON(w, http.StatusCreated, pricingDecisionView{
		ID:              stored.ID,
		ModelAlias:      stored.ModelAlias,
		Currency:        stored.Currency,
		Unit:            stored.Unit,
		PriceMicros:     stored.PriceMicros,
		USD:             float64(stored.PriceMicros) / 1_000_000,
		Resolution:      stored.Resolution,
		DurationSeconds: stored.DurationSeconds,
		Mode:            stored.Mode,
		Audio:           stored.Audio,
		Rationale:       stored.Rationale,
		DecidedAt:       stored.DecidedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Warnings:        warnings,
	})
}

// retailFloorWarnings looks up the variant's credits from the latest
// higgs_job_set_costs rules, computes `credits × reference_unit_cost ×
// markup`, and attaches a warning when retail is below that floor. All
// error paths degrade gracefully — the WRITE has already succeeded, so
// the warning array simply captures whichever signal is available.
//
// The lookup mirrors pricing_matrix.go's key semantics but tailored to
// a single variant: we need at most one matching rule. The kling3_0
// mode-folding case (contract §4.1) is handled by treating an empty
// decision mode as "match any rule mode for this resolution", since
// the pricing_matrix fan-out already collapsed pro/std into resolution.
func (h *ModelsHandler) retailFloorWarnings(r *http.Request, spec *domain.ModelSpec, stored domain.ModelPriceDecision) []pricingDecisionWarning {
	if h.FloorMarkupMultiplier <= 0 || spec == nil {
		return nil
	}
	// Phase-2: unit cost may come from purchase_batches (dynamic
	// weighted average) or fall back to config. `dynamic` flag is
	// carried into the warning payload so the operator UI can badge
	// the source. If BOTH sources are zero — no batches AND no
	// config value — skip the check entirely.
	unitCost, dynamic := h.effectiveFloorUnitCost(r.Context())
	if unitCost <= 0 {
		return nil
	}
	rules, err := h.Pricing.ListLatestRules(r.Context(), "higgs_job_set_costs")
	if err != nil || len(rules) == 0 {
		return []pricingDecisionWarning{{
			Code:    "cost_rule_missing",
			Message: "no higgs_job_set_costs rules available; retail floor check skipped",
		}}
	}
	credits, matched := lookupVariantCredits(rules, spec.JST, stored)
	if !matched {
		return []pricingDecisionWarning{{
			Code: "cost_rule_missing",
			Message: fmt.Sprintf(
				"no cost rule matches (jst=%s, resolution=%s, duration=%d, mode=%s, audio=%s); retail floor check skipped",
				spec.JST, stored.Resolution, stored.DurationSeconds, stored.Mode, stored.Audio),
		}}
	}
	// credits × unit_cost_micros × markup. credits is in hundredths, so
	// divide by 100 at the end. Kept as int64 arithmetic to avoid
	// float drift on the wire number.
	floorMicros := credits * unitCost / 100
	floorMicros = int64(float64(floorMicros) * h.FloorMarkupMultiplier)
	if stored.PriceMicros >= floorMicros {
		return nil
	}
	source := "config"
	if dynamic {
		source = "purchase_batches"
	}
	return []pricingDecisionWarning{{
		Code: "retail_below_floor",
		Message: fmt.Sprintf(
			"retail %.6f USD is below floor %.6f USD (%.2f credits × %.6f USD/credit × %.2f markup, source=%s)",
			float64(stored.PriceMicros)/1_000_000,
			float64(floorMicros)/1_000_000,
			float64(credits)/100,
			float64(unitCost)/1_000_000,
			h.FloorMarkupMultiplier,
			source,
		),
		FloorMicros:       floorMicros,
		RetailMicros:      stored.PriceMicros,
		ReferenceUnitCost: unitCost,
		MarkupMultiplier:  fmt.Sprintf("%.2f", h.FloorMarkupMultiplier),
		VariantCredits:    fmt.Sprintf("%.2f", float64(credits)/100),
	}}
}

// lookupVariantCredits picks the cost rule that best matches a
// decision variant and returns its credits (as int64 hundredths).
//
// Two-pass strategy:
//  1. Exact match on (jst, resolution, duration, mode, audio, unit).
//  2. Kling3-style fallback: match on (jst, audio, duration, unit) when
//     the rule's resolution is empty (resolution-agnostic upstream
//     payload) and the decision has a resolution. Prefer the rule
//     whose mode aligns with the decision resolution's fold
//     (1080p→pro, 720p→std) so pro/std sub-tiers pick the right row.
//
// Returns (0, false) when no rule matches — caller emits a
// cost_rule_missing warning rather than fabricating a floor.
func lookupVariantCredits(rules []domain.ModelCostRule, jst string, d domain.ModelPriceDecision) (int64, bool) {
	// Pass 1: exact match. Common path for anything that isn't kling3_0.
	for _, rule := range rules {
		if rule.JST != jst {
			continue
		}
		if rule.Resolution == d.Resolution &&
			rule.DurationSeconds == d.DurationSeconds &&
			rule.Mode == d.Mode &&
			rule.Audio == d.Audio &&
			rule.Unit == d.Unit {
			return rule.CreditsHundredths, true
		}
	}
	// Pass 2: resolution-agnostic + mode-fold fallback. Only relevant
	// when the decision carries a resolution (else there's nothing to
	// fold from) and the rule's resolution is empty.
	if d.Resolution == "" {
		return 0, false
	}
	expectedMode := ""
	switch d.Resolution {
	case "1080p":
		expectedMode = "pro"
	case "720p":
		expectedMode = "std"
	}
	// First try: rule.Mode == expectedMode + audio/duration match.
	for _, rule := range rules {
		if rule.JST != jst || rule.Resolution != "" {
			continue
		}
		if rule.Audio == d.Audio &&
			rule.DurationSeconds == d.DurationSeconds &&
			rule.Mode == expectedMode {
			return rule.CreditsHundredths, true
		}
	}
	// Second try: any mode. Better a rough floor than none.
	for _, rule := range rules {
		if rule.JST != jst || rule.Resolution != "" {
			continue
		}
		if rule.Audio == d.Audio &&
			rule.DurationSeconds == d.DurationSeconds {
			return rule.CreditsHundredths, true
		}
	}
	return 0, false
}
