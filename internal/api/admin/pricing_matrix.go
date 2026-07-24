package admin

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"
)

type pricingDimensions struct {
	Resolution      string `json:"resolution"`
	DurationSeconds int    `json:"duration_seconds"`
	Mode            string `json:"mode"`
	Audio           string `json:"audio"`
	Unit            string `json:"unit"`
}

type pricingMatrixRow struct {
	Dimensions  pricingDimensions     `json:"dimensions"`
	Higgs       []pricingHiggsValue   `json:"higgs"`
	PlanCosts   []pricingPlanCost     `json:"plan_costs"`
	OfficialAPI []pricingOfficialValue `json:"official_api,omitempty"`
	FinalPrice  *pricingFinalValue    `json:"final_price,omitempty"`
	// SuggestedPrice is server-computed advisory: floor_reference_unit_cost
	// × variant_credits × markup_multiplier (contract §10). UI shows it
	// beside the operator's custom input; nil when we can't compute it.
	SuggestedPrice *pricingSuggestedValue `json:"suggested_price,omitempty"`
}

type pricingSuggestedValue struct {
	USD               float64 `json:"usd"`
	FloorUSD          float64 `json:"floor_usd"`
	MarkupMultiplier  float64 `json:"markup_multiplier"`
	ReferenceCostUSD  float64 `json:"reference_cost_usd"`
	VariantCredits    float64 `json:"variant_credits"`
}

type pricingHiggsValue struct {
	Credits         float64 `json:"credits"`
	OriginalCredits float64 `json:"original_credits,omitempty"`
	Component       string  `json:"component"`
}

type pricingPlanCost struct {
	PlanType  string  `json:"plan_type"`
	PlanName  string  `json:"plan_name"`
	Component string  `json:"component"`
	USD       float64 `json:"usd"`
}

type pricingOfficialValue struct {
	Provider  string  `json:"provider"`
	USD       float64 `json:"usd"`
	SourceURL string  `json:"source_url"`
	Region    string  `json:"region"`              // "intl" | "cn"
	Estimated bool    `json:"estimated,omitempty"` // true = derived / not from official page
	Mode      string  `json:"mode,omitempty"`      // sub-tier tag for multi-mode Kling rows
}

type pricingFinalValue struct {
	USD       float64 `json:"usd"`
	Rationale string  `json:"rationale,omitempty"`
}

// pricingKey collapses a variant to the dimensions we treat as the row's
// identity. Resolution is the primary tier (matches how downstream
// new-api and Kling's own docs organize their pricing). audio + unit +
// duration disambiguate variants within a resolution. `mode` is
// intentionally not part of the key: some upstream sources (Higgs
// /job-sets/costs on kling3_0) return a `mode: pro|std` subdivision
// whose exact meaning is not documented, so we surface it as a `component`
// tag on individual Higgs credit / plan_cost entries instead of splitting
// the row. That keeps the table row-count aligned with the primary tier
// while preserving the sub-tier information without pinning a semantics
// we can't verify.
func pricingKey(d pricingDimensions) string {
	return d.Resolution + "\x00" + d.Audio + "\x00" + d.Unit + "\x00" + strconv.Itoa(d.DurationSeconds)
}

// PricingMatrix serves the operator-only, lossless comparison view for one
// model. Sources remain separate; a missing final price means no decision has
// been approved yet.
func (h *ModelsHandler) PricingMatrix(w http.ResponseWriter, r *http.Request) {
	if h.Registry == nil || h.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "pricing_unavailable", "pricing matrix is not configured")
		return
	}
	alias := chi.URLParam(r, "alias")
	spec, err := h.Registry.Resolve(alias)
	if err != nil || spec == nil {
		writeErr(w, http.StatusNotFound, "model_not_found", "model not found")
		return
	}

	rules, err := h.Pricing.ListLatestRules(r.Context(), "higgs_job_set_costs")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}
	rates, err := h.Pricing.ListPlanCreditRates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}
	official, err := h.Pricing.ListOfficialPrices(r.Context(), spec.Alias)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}
	decisions, err := h.Pricing.ListPriceDecisions(r.Context(), spec.Alias)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}

	rows := make(map[string]*pricingMatrixRow)
	ensure := func(d pricingDimensions) *pricingMatrixRow {
		key := pricingKey(d)
		if rows[key] == nil {
			rows[key] = &pricingMatrixRow{Dimensions: d, Higgs: []pricingHiggsValue{}, PlanCosts: []pricingPlanCost{}}
		}
		return rows[key]
	}

	// Pass 1: official observations define the primary tier grid — a
	// resolution × audio matrix that mirrors how Kling / Kuaishou docs
	// and downstream new-api organize pricing. Populate their rows first
	// so pass 2 can fan Higgs rules out onto every resolution. Now that
	// observations carry a region axis (intl / cn) we keep every price
	// on the row rather than collapsing to the first-seen — the UI
	// renders one column per region.
	for _, price := range official {
		d := pricingDimensions{price.Resolution, price.DurationSeconds, "", price.Audio, price.Unit}
		row := ensure(d)
		row.OfficialAPI = append(row.OfficialAPI, pricingOfficialValue{
			Provider:  price.Provider,
			USD:       float64(price.PriceMicros) / 1_000_000,
			SourceURL: price.SourceURL,
			Region:    price.Region,
			Estimated: price.Estimated,
			Mode:      price.Mode,
		})
	}

	// Collect every (resolution, unit) tier the official grid already
	// knows, keyed by audio + duration only. When a Higgs rule arrives
	// with an empty resolution (e.g. kling3_0 whose /job-sets/costs is
	// resolution-agnostic), we fan it out onto each of those tiers so
	// the operator sees credits + plan cost on the same row as the
	// upstream API price.
	//
	// The unit is deliberately NOT part of the match key: Higgs
	// /job-sets/costs frequently ships a placeholder like
	// "upstream_unspecified" for kling3_0 while the official API price
	// is denominated in per_second. Falling back to audio + duration
	// lets the fan-out succeed and we take the unit from the official
	// tier since it is the authoritative one.
	type officialTier struct {
		resolution string
		unit       string
	}
	officialTiers := map[string][]officialTier{}
	tierKey := func(audio string, duration int) string {
		return audio + "\x00" + strconv.Itoa(duration)
	}
	seenTier := map[string]bool{}
	for _, price := range official {
		k := tierKey(price.Audio, price.DurationSeconds)
		fp := k + "\x00" + price.Resolution + "\x00" + price.Unit
		if seenTier[fp] {
			continue
		}
		seenTier[fp] = true
		officialTiers[k] = append(officialTiers[k], officialTier{price.Resolution, price.Unit})
	}

	// Pass 2: Higgs credits + plan cost fanout.
	for _, rule := range rules {
		if rule.JST != spec.JST {
			continue
		}
		// Component tag now carries the upstream-supplied sub-tier
		// (e.g. Higgs `mode: pro|std` for kling3_0) so it survives
		// even though it no longer partitions the row.
		component := rule.Component
		if rule.Mode != "" {
			if component == "" {
				component = rule.Mode
			} else {
				component = rule.Mode + "/" + component
			}
		}
		attach := func(target pricingDimensions) {
			row := ensure(target)
			row.Higgs = append(row.Higgs, pricingHiggsValue{
				Credits:         float64(rule.CreditsHundredths) / 100,
				OriginalCredits: float64(rule.OriginalCreditsHundredths) / 100,
				Component:       component,
			})
			for _, rate := range rates {
				costMicros := rule.CreditsHundredths * rate.UnitCostMicros / 100
				row.PlanCosts = append(row.PlanCosts, pricingPlanCost{
					PlanType: rate.PlanType, PlanName: rate.PlanName,
					Component: component,
					USD:       float64(costMicros) / 1_000_000,
				})
			}
		}
		if rule.Resolution == "" {
			// Fan out: this rule is resolution-agnostic in the upstream
			// payload. Attach to every (resolution, unit) tier the
			// official grid knows about under the same audio/duration.
			// Adopt the official tier's unit so the row keys align.
			targets := officialTiers[tierKey(rule.Audio, rule.DurationSeconds)]
			if len(targets) == 0 {
				// No official observation to anchor to — keep the
				// "any resolution" row so the credits/plan_costs
				// are still visible.
				attach(pricingDimensions{"", rule.DurationSeconds, "", rule.Audio, rule.Unit})
				continue
			}
			for _, tier := range targets {
				attach(pricingDimensions{tier.resolution, rule.DurationSeconds, "", rule.Audio, tier.unit})
			}
		} else {
			attach(pricingDimensions{rule.Resolution, rule.DurationSeconds, "", rule.Audio, rule.Unit})
		}
	}
	// Pass 3: operator decisions.
	for _, decision := range decisions {
		d := pricingDimensions{decision.Resolution, decision.DurationSeconds, "", decision.Audio, decision.Unit}
		row := ensure(d)
		if row.FinalPrice == nil {
			row.FinalPrice = &pricingFinalValue{float64(decision.PriceMicros) / 1_000_000, decision.Rationale}
		}
	}

	// Pass 4: server-computed suggested_price using the same floor rule
	// the POST /pricing-decisions endpoint validates against
	// (contract §10). variant_credits = max Higgs credits observed on
	// the row (pro > std when both exist, so the suggested price covers
	// the more expensive sub-tier). floor = credits × reference × markup.
	// UI renders the suggestion beside the operator's custom input;
	// nothing is written to model_price_decisions from here.
	refUnitCost := h.FloorReferenceUnitCostMicros
	markup := h.FloorMarkupMultiplier
	for _, row := range rows {
		if refUnitCost <= 0 || markup <= 0 || len(row.Higgs) == 0 {
			continue
		}
		var maxCredits float64
		for _, hv := range row.Higgs {
			if hv.Credits > maxCredits {
				maxCredits = hv.Credits
			}
		}
		if maxCredits <= 0 {
			continue
		}
		refUSD := float64(refUnitCost) / 1_000_000
		floorUSD := maxCredits * refUSD
		row.SuggestedPrice = &pricingSuggestedValue{
			USD:              floorUSD * markup,
			FloorUSD:         floorUSD,
			MarkupMultiplier: markup,
			ReferenceCostUSD: refUSD,
			VariantCredits:   maxCredits,
		}
	}

	data := make([]pricingMatrixRow, 0, len(rows))
	for _, row := range rows {
		data = append(data, *row)
	}
	sort.Slice(data, func(i, j int) bool {
		a, b := data[i].Dimensions, data[j].Dimensions
		if resolutionRank(a.Resolution) != resolutionRank(b.Resolution) {
			return resolutionRank(a.Resolution) < resolutionRank(b.Resolution)
		}
		if a.Audio != b.Audio {
			return a.Audio < b.Audio
		}
		if a.Mode != b.Mode {
			return a.Mode < b.Mode
		}
		return a.DurationSeconds < b.DurationSeconds
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object":      "pricing.matrix",
		"model_alias": spec.Alias,
		"jst":         spec.JST,
		"min_plan":    spec.MinPlan,
		"rows":        data,
	})
}

func resolutionRank(value string) int {
	switch value {
	case "480p":
		return 480
	case "720p":
		return 720
	case "1080p":
		return 1080
	case "2k":
		return 2000
	case "4k":
		return 4000
	default:
		return 9999
	}
}
