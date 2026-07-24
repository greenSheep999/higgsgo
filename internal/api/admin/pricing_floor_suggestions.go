package admin

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// FloorSuggestions serves GET /admin/pricing/floor-suggestions.
//
// A per-(alias × variant) planning table for the pricing operator. Each
// row combines four inputs the operator would otherwise have to open
// four browser tabs to see:
//
//  1. **Floor** — contract §10's `credits × reference_unit_cost × markup`
//     lower bound derived from the latest `higgs_job_set_costs` rule
//     for the variant. If we cannot compute it (rule missing, config
//     disabled, unit mismatch) the field is null and `floor_reason`
//     names which input was missing so the operator knows where to
//     look.
//  2. **Official low/mid/high** — min/median/max across all
//     `official_price_observations` rows for the same
//     (alias, resolution, mode, audio, duration) tuple. Multiple
//     providers per model are aggregated by picking min/median/max
//     of the provider distribution.
//  3. **Current** — the latest operator decision from
//     `model_price_decisions` for the tuple, if any. When absent, the
//     operator has not priced this variant yet.
//  4. **Recommended** — `max(floor, official_median)`, expressing
//     "never sell below the cost floor, but also never lose the
//     margin implied by the market median". Falls back to floor when
//     official prices are missing, or to official_median when floor
//     cannot be computed.
//
// Returned shape:
//
//	{
//	  "generated_at": "<RFC3339>",
//	  "config": {
//	    "reference_unit_cost_micros": 27500,
//	    "markup_multiplier": "1.80",
//	    "enabled": true
//	  },
//	  "rows": [
//	    {
//	      "model_alias": "kling-3",
//	      "jst": "kling3_0",
//	      "resolution": "720p",
//	      "duration_seconds": 5,
//	      "mode": "",
//	      "audio": "on",
//	      "unit": "per_second",
//	      "credits":                1.26,
//	      "floor_micros":           62_370,     // 126 × 27_500 × 1.8 / 100
//	      "floor_reason":           "",
//	      "official_low_micros":    126_000,
//	      "official_mid_micros":    126_000,
//	      "official_high_micros":   126_000,
//	      "current_micros":         150_000,
//	      "recommended_micros":     126_000,
//	      "current_vs_floor":       "above",   // above | at | below | unknown
//	      "current_vs_official":    "above",
//	      "providers":              ["Kuaishou Kling"]
//	    }, ...
//	  ]
//	}
//
// The endpoint is intentionally read-only and non-paginated: even at
// several hundred aliases × ~6 variants each, the response fits in
// well under 1 MB and the operator UI wants a single table.
func (h *ModelsHandler) FloorSuggestions(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable",
			"pricing persistence is not configured")
		return
	}
	ctx := r.Context()

	// Cost rules index (`higgs_job_set_costs`) → JST × variant → credits.
	// A missing entry surfaces as floor_reason="cost_rule_missing".
	rules, err := h.Pricing.ListLatestRules(ctx, "higgs_job_set_costs")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Official price index → (alias, res, mode, audio, dur) → []price.
	// Multiple providers stack under one key so we can take min/median/max.
	official, err := h.Pricing.ListAllOfficialPrices(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Latest decision per variant, used for the "current" column.
	decisions, err := h.Pricing.ListLatestPriceDecisions(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Phase-2: unit cost is dynamic (weighted average across
	// purchase_batches when data present, config fallback when
	// not). Compute once here and pass into buildFloorSuggestionRows
	// so every row uses the SAME number — otherwise the operator
	// sees inconsistent floors depending on when each row was
	// rendered.
	unitCost, dynamic := h.effectiveFloorUnitCost(ctx)
	rows := buildFloorSuggestionRows(h, rules, official, decisions, unitCost)

	enabled := unitCost > 0 && h.FloorMarkupMultiplier > 0
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": nowUTCString(ctx),
		"config": map[string]any{
			"reference_unit_cost_micros": unitCost,
			"markup_multiplier":          h.FloorMarkupMultiplier,
			"enabled":                    enabled,
			// "dynamic" is true when unit cost came from
			// purchase_batches, false when it fell back to
			// the config constant. UI badges the source.
			"dynamic": dynamic,
			// Original config value is still surfaced so ops
			// can spot when the two diverge sharply.
			"config_fallback_micros": h.FloorReferenceUnitCostMicros,
		},
		"rows": rows,
	})
}

// floorSuggestionRow is one variant's precomputed row. Kept as a bag of
// scalars matching the response wire shape — no json.Marshal indirection
// through a helper type, because the fields are near-identical.
type floorSuggestionRow struct {
	ModelAlias         string   `json:"model_alias"`
	JST                string   `json:"jst"`
	Resolution         string   `json:"resolution"`
	DurationSeconds    int      `json:"duration_seconds"`
	Mode               string   `json:"mode"`
	Audio              string   `json:"audio"`
	Unit               string   `json:"unit"`
	Credits            float64  `json:"credits"`
	FloorMicros        *int64   `json:"floor_micros"`
	FloorReason        string   `json:"floor_reason,omitempty"`
	OfficialLowMicros  *int64   `json:"official_low_micros"`
	OfficialMidMicros  *int64   `json:"official_mid_micros"`
	OfficialHighMicros *int64   `json:"official_high_micros"`
	CurrentMicros      *int64   `json:"current_micros"`
	RecommendedMicros  *int64   `json:"recommended_micros"`
	CurrentVsFloor     string   `json:"current_vs_floor"`
	CurrentVsOfficial  string   `json:"current_vs_official"`
	Providers          []string `json:"providers"`
}

// buildFloorSuggestionRows is the pure-function core, split out so
// tests can drive it without spinning up a store fake.
//
// Strategy: iterate every variant an official observation OR a decision
// has ever priced. Cost rules alone don't drive rows (a JST can have
// credits but no variant grid — the operator wouldn't be pricing it
// until at least one observation lands). For each variant, look up
// credits from the rule matrix, gather official prices across
// providers, and pull the operator's current decision if any.
func buildFloorSuggestionRows(
	h *ModelsHandler,
	rules []domain.ModelCostRule,
	official []domain.OfficialPriceObservation,
	decisions []domain.ModelPriceDecision,
	unitCostMicros int64,
) []floorSuggestionRow {

	// Index rules by (JST, resolution, duration, mode, audio, unit).
	// The exact key mirrors the lookup pricing_decisions.go uses for
	// the retail_below_floor warning; keeping semantics in sync means
	// this endpoint's floor matches what POST decisions warns against.
	ruleIdx := make(map[ruleIndexKey]int64, len(rules))
	for _, rule := range rules {
		k := ruleIndexKey{jst: rule.JST, res: rule.Resolution, mode: rule.Mode, audio: rule.Audio, unit: rule.Unit, dur: rule.DurationSeconds}
		if _, exists := ruleIdx[k]; !exists {
			ruleIdx[k] = rule.CreditsHundredths
		}
	}

	// JST-per-alias table via the registry so a row's official row
	// (keyed by alias) can find its cost rule (keyed by JST).
	aliasToJST := aliasToJSTMap(h)

	// Bucket observations by their (alias × variant) tuple. Multiple
	// providers per bucket → we aggregate min/median/max at emit
	// time. Bucket keeps the raw list so median computation stays
	// deterministic (sort + pick).
	//
	// variantKey uses the same field layout as ruleIndexKey (minus
	// the leading jst) so lookupCreditsForFloor can trivially copy
	// the shared fields over into a ruleIndexKey at lookup time.
	type variantKey struct {
		alias, res, mode, audio, unit string
		dur                           int
	}
	obsBuckets := make(map[variantKey][]domain.OfficialPriceObservation, 64)
	for _, o := range official {
		if o.PriceMicros <= 0 {
			continue
		}
		k := variantKey{o.ModelAlias, o.Resolution, o.Mode, o.Audio, o.Unit, o.DurationSeconds}
		obsBuckets[k] = append(obsBuckets[k], o)
	}

	// Decisions index → same variantKey → decision. Latest-per-
	// variant across all aliases already guaranteed by the store.
	decIdx := make(map[variantKey]domain.ModelPriceDecision, len(decisions))
	for _, d := range decisions {
		if d.PriceMicros <= 0 {
			continue
		}
		k := variantKey{d.ModelAlias, d.Resolution, d.Mode, d.Audio, d.Unit, d.DurationSeconds}
		decIdx[k] = d
	}

	// The set of variantKeys we need rows for is the union of
	// observation keys and decision keys. A decision without an
	// official observation is legitimate (the operator priced ahead
	// of the market-data feed catching up).
	keys := make(map[variantKey]struct{}, len(obsBuckets)+len(decIdx))
	for k := range obsBuckets {
		keys[k] = struct{}{}
	}
	for k := range decIdx {
		keys[k] = struct{}{}
	}

	// Sort keys deterministically so the operator can diff between
	// pulls. Order: alias asc → resolution rank desc → duration asc
	// → mode asc → audio asc.
	orderedKeys := make([]variantKey, 0, len(keys))
	for k := range keys {
		orderedKeys = append(orderedKeys, k)
	}
	sort.Slice(orderedKeys, func(i, j int) bool {
		a, b := orderedKeys[i], orderedKeys[j]
		if a.alias != b.alias {
			return a.alias < b.alias
		}
		if a.res != b.res {
			return floorResolutionRank(a.res) > floorResolutionRank(b.res)
		}
		if a.dur != b.dur {
			return a.dur < b.dur
		}
		if a.mode != b.mode {
			return a.mode < b.mode
		}
		return a.audio < b.audio
	})

	rows := make([]floorSuggestionRow, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		jst := aliasToJST[k.alias]
		row := floorSuggestionRow{
			ModelAlias:      k.alias,
			JST:             jst,
			Resolution:      k.res,
			DurationSeconds: k.dur,
			Mode:            k.mode,
			Audio:           k.audio,
			Unit:            k.unit,
		}

		// Credits + floor. Look up the rule the same way POST
		// decisions does: exact match first, then kling3_0
		// resolution-agnostic + mode-fold fallback. Copy the
		// shared-shape fields into a ruleIndexKey; jst is filled
		// inside the lookup because the caller knows only alias.
		credits := lookupCreditsForFloor(ruleIdx, ruleIndexKey{
			res: k.res, mode: k.mode, audio: k.audio, unit: k.unit, dur: k.dur,
		}, jst)
		row.Credits = float64(credits) / 100
		if credits > 0 && unitCostMicros > 0 && h.FloorMarkupMultiplier > 0 {
			// credits (hundredths) × unit_cost / 100 × markup.
			floor := credits * unitCostMicros / 100
			floor = int64(float64(floor) * h.FloorMarkupMultiplier)
			row.FloorMicros = &floor
		} else {
			switch {
			case unitCostMicros <= 0 || h.FloorMarkupMultiplier <= 0:
				row.FloorReason = "floor_disabled"
			case credits == 0:
				row.FloorReason = "cost_rule_missing"
			default:
				row.FloorReason = "unknown"
			}
		}

		// Official low/mid/high.
		if bucket := obsBuckets[k]; len(bucket) > 0 {
			prices := make([]int64, 0, len(bucket))
			providers := make(map[string]struct{}, len(bucket))
			for _, o := range bucket {
				prices = append(prices, o.PriceMicros)
				if o.Provider != "" {
					providers[o.Provider] = struct{}{}
				}
			}
			sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
			low := prices[0]
			high := prices[len(prices)-1]
			mid := prices[len(prices)/2]
			row.OfficialLowMicros = &low
			row.OfficialMidMicros = &mid
			row.OfficialHighMicros = &high
			providerList := make([]string, 0, len(providers))
			for p := range providers {
				providerList = append(providerList, p)
			}
			sort.Strings(providerList)
			row.Providers = providerList
		}

		// Current decision.
		if d, ok := decIdx[k]; ok {
			p := d.PriceMicros
			row.CurrentMicros = &p
		}

		// Recommended = max(floor, official_mid). Both nil → nil.
		row.RecommendedMicros = recommendPrice(row.FloorMicros, row.OfficialMidMicros)

		// Comparison strings for UI.
		row.CurrentVsFloor = compareOptional(row.CurrentMicros, row.FloorMicros)
		row.CurrentVsOfficial = compareOptional(row.CurrentMicros, row.OfficialMidMicros)

		rows = append(rows, row)
	}
	return rows
}

// lookupCreditsForFloor mirrors the two-pass strategy in
// internal/api/admin/pricing_decisions.go (retailFloorWarnings) so this
// endpoint's floor number is identical to the warning the operator
// sees on save. Exact key first; kling3_0 resolution-agnostic
// fallback second (mode fold: 1080p→pro, 720p→std).
type ruleIndexKey = struct {
	jst, res, mode, audio, unit string
	dur                         int
}

func lookupCreditsForFloor(
	ruleIdx map[ruleIndexKey]int64,
	variant ruleIndexKey,
	jst string,
) int64 {
	if jst == "" {
		return 0
	}
	// Pass 1: exact match. variant.jst is unset (variantKey ~= ruleKey
	// shape but keyed off alias); rebuild the lookup key with jst.
	exact := variant
	exact.jst = jst
	if c, ok := ruleIdx[exact]; ok {
		return c
	}
	// Pass 2: kling3_0 resolution-agnostic fallback. Only relevant
	// when the variant carries a resolution AND we have a
	// resolution-empty rule under the same JST.
	if variant.res == "" {
		return 0
	}
	expectedMode := ""
	switch variant.res {
	case "1080p":
		expectedMode = "pro"
	case "720p":
		expectedMode = "std"
	}
	// Try mode+audio+dur match.
	fallback := ruleIndexKey{jst: jst, res: "", mode: expectedMode, audio: variant.audio, unit: variant.unit, dur: variant.dur}
	if c, ok := ruleIdx[fallback]; ok {
		return c
	}
	// Loosest fallback: any mode for the same audio+duration.
	for key, credits := range ruleIdx {
		if key.jst != jst || key.res != "" {
			continue
		}
		if key.audio == variant.audio && key.dur == variant.dur {
			return credits
		}
	}
	return 0
}

// recommendPrice returns max(floor, mid) with the semantics:
//   - both nil → nil
//   - one nil → the other
//   - both set → the larger
//
// This matches the endpoint's stated "never sell below the cost floor
// but never below the market median either" rule.
func recommendPrice(floor, mid *int64) *int64 {
	if floor == nil && mid == nil {
		return nil
	}
	if floor == nil {
		v := *mid
		return &v
	}
	if mid == nil {
		v := *floor
		return &v
	}
	v := *floor
	if *mid > v {
		v = *mid
	}
	return &v
}

// compareOptional returns a short string tag describing how `a`
// compares to `b`. Returns "unknown" when either side is nil so the
// UI can render a distinct cell (blank/dash) instead of implying an
// "above/at/below" verdict on missing data.
func compareOptional(a, b *int64) string {
	if a == nil || b == nil {
		return "unknown"
	}
	if *a > *b {
		return "above"
	}
	if *a < *b {
		return "below"
	}
	return "at"
}

// aliasToJSTMap collapses the registry into an alias→JST lookup so
// per-alias observation buckets can be joined against JST-keyed cost
// rules. Includes ExtraAliases so a decision that used an old alias
// (e.g. "kling-v3") still resolves to the canonical JST.
func aliasToJSTMap(h *ModelsHandler) map[string]string {
	out := make(map[string]string)
	if h.Registry == nil {
		return out
	}
	for _, spec := range h.Registry.List(ports.ModelFilter{IncludeUnstable: true, IncludeDeprecated: true}) {
		if spec == nil || spec.JST == "" {
			continue
		}
		out[spec.Alias] = spec.JST
		for _, alt := range spec.ExtraAliases {
			out[alt] = spec.JST
		}
	}
	return out
}

// floorResolutionRank mirrors resolutionRank in v1/downstream_pricing.go
// so the sort order in this endpoint matches the DSL's tier order.
// Kept as a local copy to avoid a cross-package dependency on v1
// (this file lives in admin/).
func floorResolutionRank(r string) int {
	switch r {
	case "4k", "2160p":
		return 400
	case "1440p", "2k":
		return 300
	case "1080p":
		return 200
	case "768p":
		return 150
	case "720p":
		return 100
	case "512p":
		return 80
	case "480p":
		return 50
	}
	return 0
}

// nowUTCString stamps the response's generated_at field. Kept as a
// thin wrapper (rather than inlining time.Now()) so a future test
// hook can override it deterministically.
func nowUTCString(_ context.Context) string {
	return time.Now().UTC().Format(time.RFC3339)
}
