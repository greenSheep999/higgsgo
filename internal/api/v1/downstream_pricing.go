package v1

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// HandleDownstreamPricing serves GET /api/pricing.
//
// Contract §6.2 (post-2026-07-24 semantics flip): this endpoint is an
// AGGREGATOR of provider-public "official API" prices. Downstream
// (new-api's controller/ratio_sync.go) consumes it exactly like it
// consumes the built-in "basellm.github.io" or "models.dev" presets:
// the numbers land in the operator's `model_price` field, which is
// then multiplied by group_ratio to derive the customer-facing price.
//
// higgsgo is the source of truth here because higgsfield-register/
// docs/raw-pricing/ maintains hand-verified provider pages that the
// public presets don't cover (Kuaishou Kling, Kuaishou Kolors, Higgs
// self, etc. — especially audio/video pricing which basellm and
// models.dev do not scrape).
//
// A higgsgo operator MAY override an entry via model_price_decisions
// when we want the downstream `model_price` to be different from the
// raw official page — the typical case being "back-solve so that
// group_ratio × model_price ≥ our internal retail target". Such
// overrides are picked over the raw observation for the same
// (alias, resolution, audio, mode) tuple.
//
// Fields on each data[] row:
//
//	model_name   — Higgs public alias
//	quota_type   — 2 (tiered_expr) for models with ≥2 tiers, 1 (fixed)
//	               for text-only single-price models. new-api handles
//	               both in ratio_sync.go:381-425.
//	billing_mode — "tiered_expr" when quota_type=2, otherwise omitted
//	billing_expr — DSL, see §6.2 grammar (quota_type=2 only)
//	model_price  — flat USD price (quota_type=1 only)
//	lifecycle    — {"status":"active"} today; §7 grows deprecation edges
//
// Rows with no priced variant are OMITTED (contract §3.1). Downstream
// falls back to its own defaults / other preset URLs for them.
//
// Auth: sk-hg-* APIKeyAuth (provisional; see §6 auth clarification).
func (h *Handler) HandleDownstreamPricing(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing_store_unavailable",
			"pricing persistence is not configured")
		return
	}

	// Data source 1: raw provider prices from official_price_observations
	// (fed by operator scrapes of docs/raw-pricing/*.md into migrations
	// 029+). This is the *default* value emitted downstream.
	observations, err := h.Pricing.ListAllOfficialPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}

	// Data source 2: operator overrides (model_price_decisions).
	// These represent "back-solved" numbers where higgsgo operator wants
	// the downstream model_price to differ from the raw official page.
	// Typical trigger: raw × new-api group_ratio ends up below our
	// internal retail target (see model_price_decisions.target_retail_*
	// fields for the intent). When an override exists for an
	// (alias, resolution, audio, mode) tuple it fully replaces the
	// observation for that tuple in the emitted feed.
	overrides, err := h.Pricing.ListLatestPriceDecisions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}

	// Optional single-alias filter — case-insensitive so callers don't
	// have to remember the canonical form.
	filter := strings.TrimSpace(r.URL.Query().Get("model"))
	byAlias := mergeObservationsAndOverrides(observations, overrides, filter)

	// item is the downstream wire shape (per model_name).
	// official_price_micros carries the model-level fallback official
	// price for consumers that render a single "N% off vs official"
	// badge on the model card. For tiered models, each tier() call
	// in billing_expr additionally carries `official_micros=NNNN` in
	// its note so per-tier discount computations work too.
	type item struct {
		ModelName           string         `json:"model_name"`
		QuotaType           int            `json:"quota_type"`
		BillingMode         string         `json:"billing_mode"`
		BillingExpr         string         `json:"billing_expr"`
		ModelPrice          float64        `json:"model_price,omitempty"`
		OfficialPriceMicros int64          `json:"official_price_micros,omitempty"`
		Lifecycle           map[string]any `json:"lifecycle,omitempty"`
	}
	data := make([]item, 0, len(byAlias))
	// Deterministic order so downstream diffs are stable across calls.
	aliases := make([]string, 0, len(byAlias))
	for alias := range byAlias {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		rows := byAlias[alias]
		expr := buildBillingExpr(rows)
		if expr == "" {
			continue
		}
		// Model-level official price: the cheapest tier's official
		// value. Rationale: the "N% off" badge on a model card is
		// almost always "starts from N% off vs official cheapest",
		// which matches new-api's computeModelBestDiscount intent.
		var modelOfficial int64
		for _, r := range rows {
			if r.OfficialPriceMicros <= 0 {
				continue
			}
			// Fold per-second official price × duration to align
			// with tier fixed_micros (both are full-call totals).
			off := r.OfficialPriceMicros
			if r.Unit == "per_second" && r.DurationSeconds > 0 {
				off = off * int64(r.DurationSeconds)
			}
			if modelOfficial == 0 || off < modelOfficial {
				modelOfficial = off
			}
		}
		data = append(data, item{
			ModelName:           alias,
			QuotaType:           2,
			BillingMode:         "tiered_expr",
			BillingExpr:         expr,
			OfficialPriceMicros: modelOfficial,
			Lifecycle:           map[string]any{"status": "active"},
		})
	}

	// Downstream ratio_sync is polled periodically; a short public
	// cache is safe. Kept below the /official-api 6h floor because
	// decisions change more frequently than official-price imports.
	w.Header().Set("Cache-Control", "public, max-age=300")

	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"data":         data,
	})
}

// pricingRow is the internal wire-building row: an emitted decision
// (either raw observation OR operator override) paired with the
// official-page price for the same tuple. When an override exists,
// PriceMicros is the operator's back-solved value and
// OfficialPriceMicros is the *raw* provider page price for the same
// tuple; the ratio between the two is what drives the "N% off" badge
// downstream. When no override exists, both are the same number
// (ratio = 1.0, no discount).
type pricingRow struct {
	ModelAlias          string
	Unit                string
	PriceMicros         int64
	OfficialPriceMicros int64
	Resolution          string
	DurationSeconds     int
	Mode                string
	Audio               string
}

// mergeObservationsAndOverrides builds one bucket per alias, keyed by
// model_alias. For every (resolution, audio, mode, unit) variant the
// merge prefers a model_price_decisions row over the raw observation
// for the same tuple. Every emitted row also carries the *raw* official
// price for its tuple (OfficialPriceMicros) so the DSL builder can
// annotate each tier with `official_micros=NNNN` — that's how new-api
// front-end computes the per-tier discount.
//
// Observations with estimated=true (raw-pricing/*-cn.md derived rows)
// are dropped: downstream only publishes prices we captured from an
// authoritative page.
func mergeObservationsAndOverrides(
	observations []domain.OfficialPriceObservation,
	overrides []domain.ModelPriceDecision,
	filter string,
) map[string][]pricingRow {
	// Index overrides + observations by (alias, resolution, audio, mode, unit).
	type key struct{ alias, res, audio, mode, unit string }
	observationByKey := make(map[key]domain.OfficialPriceObservation, len(observations))
	for _, p := range observations {
		if p.ModelAlias == "" || p.PriceMicros <= 0 || p.Estimated {
			continue
		}
		if filter != "" && !strings.EqualFold(p.ModelAlias, filter) {
			continue
		}
		k := key{p.ModelAlias, p.Resolution, p.Audio, p.Mode, p.Unit}
		// Keep the first observation per tuple — the store already
		// sorts by observed_at DESC so the newest scrape wins.
		if _, exists := observationByKey[k]; !exists {
			observationByKey[k] = p
		}
	}
	overrideByKey := make(map[key]domain.ModelPriceDecision, len(overrides))
	for _, o := range overrides {
		if o.ModelAlias == "" || o.PriceMicros <= 0 {
			continue
		}
		if filter != "" && !strings.EqualFold(o.ModelAlias, filter) {
			continue
		}
		overrideByKey[key{o.ModelAlias, o.Resolution, o.Audio, o.Mode, o.Unit}] = o
	}

	out := make(map[string][]pricingRow, 8)
	emitted := make(map[key]bool, 8)

	// Pass 1: every observation flows through, unless an override for
	// the same tuple exists (in which case pass 2 emits the override
	// paired with this observation's official price).
	for k, p := range observationByKey {
		if _, hasOverride := overrideByKey[k]; hasOverride {
			continue
		}
		out[p.ModelAlias] = append(out[p.ModelAlias], pricingRow{
			ModelAlias:          p.ModelAlias,
			Unit:                p.Unit,
			PriceMicros:         p.PriceMicros,
			OfficialPriceMicros: p.PriceMicros, // raw = official
			Resolution:          p.Resolution,
			DurationSeconds:     p.DurationSeconds,
			Mode:                p.Mode,
			Audio:               p.Audio,
		})
		emitted[k] = true
	}

	// Pass 2: emit overrides. For each override, if there's a matching
	// observation, use that observation's PriceMicros as the official
	// price; else fall back to the override's own PriceMicros (edge
	// case — override-only variant with no scraped baseline).
	for k, o := range overrideByKey {
		if emitted[k] {
			continue
		}
		official := o.PriceMicros
		if obs, ok := observationByKey[k]; ok {
			official = obs.PriceMicros
		}
		out[o.ModelAlias] = append(out[o.ModelAlias], pricingRow{
			ModelAlias:          o.ModelAlias,
			Unit:                o.Unit,
			PriceMicros:         o.PriceMicros,
			OfficialPriceMicros: official,
			Resolution:          o.Resolution,
			DurationSeconds:     o.DurationSeconds,
			Mode:                o.Mode,
			Audio:               o.Audio,
		})
		emitted[k] = true
	}
	return out
}

// buildBillingExpr assembles the ternary chain that new-api's DSL
// parser walks at request time. Strategy (contract §6.2 + §4.4):
//
//  1. For each decision, expand into ONE tier. Per-second decisions
//     get their duration folded into fixed_micros (fixed_micros =
//     price_micros × duration_seconds), matching §4.4's "no new-api
//     backend changes required" compromise.
//  2. Emit one `has(param("resolution"),"X")` branch per resolution
//     group. Within a resolution, emit sub-branches for `audio` and
//     `mode` guards. Ordering: fixed by primary dimension, then audio,
//     then mode — matches how new-api's `groupTiersByPrimaryDim`
//     builds tier tabs.
//  3. If a decision has NO dimensions (empty resolution+audio+mode),
//     it becomes the terminal `tier("default", ...)` — no guard.
//  4. Always append the `tier("unpriced", 0, ...)` fallback so the DSL
//     evaluator never falls off the end (§6.2).
//
// The output is one line, no whitespace beyond DSL-required spaces
// around `&&` / `?` / `:` — new-api's parser is whitespace-tolerant but
// deterministic single-line output diffs cleanly.
func buildBillingExpr(rows []pricingRow) string {
	if len(rows) == 0 {
		return ""
	}

	// Pass 1: classify each row → tier{guards, label, fixedMicros, note}.
	// The note carries: unit · duration_seconds · official_micros. new-api
	// front-end parses these into ParsedTier.params so both per-tier and
	// per-model discount computations can find the official value.
	tiers := make([]dslTier, 0, len(rows))
	for _, d := range rows {
		fixed := d.PriceMicros
		official := d.OfficialPriceMicros
		noteParts := []string{d.Unit}
		if d.Unit == "per_second" && d.DurationSeconds > 0 {
			fixed = d.PriceMicros * int64(d.DurationSeconds)
			official = official * int64(d.DurationSeconds)
			noteParts = append(noteParts, fmt.Sprintf("duration_seconds=%d", d.DurationSeconds))
		} else if d.DurationSeconds > 0 {
			noteParts = append(noteParts, fmt.Sprintf("duration_seconds=%d", d.DurationSeconds))
		}
		if official > 0 && official != fixed {
			// Emit official_micros only when it differs from fixed —
			// otherwise the tier is at the raw official price
			// (discount ratio = 1.0, no badge to render). Saves wire
			// bytes and avoids downstream computing a no-op discount.
			noteParts = append(noteParts, fmt.Sprintf("official_micros=%d", official))
		}
		tiers = append(tiers, dslTier{
			resolution: d.Resolution,
			audio:      d.Audio,
			mode:       d.Mode,
			label:      tierLabelFor(d),
			fixed:      fixed,
			note:       strings.Join(noteParts, " · "),
		})
	}

	// Pass 2: bucket by (resolution, audio, mode). Contract §5 rule 4
	// forbids ambiguity, so any duplicate within a bucket is a data-
	// integrity issue that must be flagged rather than papered over.
	// The ListLatestPriceDecisions store query already collapses to
	// "one row per variant" so this map should never see collisions,
	// but the merge step keeps the last-wins tie-break explicit.
	unique := make(map[string]dslTier, len(tiers))
	keys := make([]string, 0, len(tiers))
	for _, t := range tiers {
		k := t.resolution + "|" + t.audio + "|" + t.mode
		if _, ok := unique[k]; !ok {
			keys = append(keys, k)
		}
		unique[k] = t
	}

	// Sort keys so the DSL is deterministic. Sort order:
	//   1. non-empty resolution before empty (empty is the "any" fallback)
	//   2. resolutions descending by numeric prefix so 4k > 1080p > 720p
	//      → downstream front-end sees "highest quality first" tabs
	//   3. audio: on > off > empty (higher-value tiers first)
	//   4. mode alphabetic
	sort.Slice(keys, func(i, j int) bool {
		ti, tj := unique[keys[i]], unique[keys[j]]
		if (ti.resolution == "") != (tj.resolution == "") {
			return ti.resolution != "" // non-empty first
		}
		if r := compareResolution(ti.resolution, tj.resolution); r != 0 {
			return r < 0
		}
		if r := compareAudio(ti.audio, tj.audio); r != 0 {
			return r < 0
		}
		return ti.mode < tj.mode
	})

	// Build inside-out: rightmost is the fallback tier, wrap each
	// preceding tier in a ternary with its guard. Left-to-right order
	// of `keys` = evaluation order (first match wins), so the LAST
	// key in `keys` sits deepest inside the ternary.
	//
	// Contract example line 104 shows this shape:
	//   has(...)"1080p" ? tier(...) : has(...)"720p" ? tier(...) : tier("unpriced", 0, ...)
	expr := fallbackTier()
	for i := len(keys) - 1; i >= 0; i-- {
		t := unique[keys[i]]
		call := fmt.Sprintf(`tier(%s, %d, %s)`, quoteNote(t.label), t.fixed, quoteNote(t.note))
		guard := buildGuard(t)
		if guard == "" {
			// Terminal tier with no dimensions — becomes the innermost
			// tier before the fallback. Uses no ternary head.
			expr = fmt.Sprintf(`%s : %s`, call, expr)
			continue
		}
		expr = fmt.Sprintf(`%s ? %s : %s`, guard, call, expr)
	}
	return expr
}

// tierLabel is the human-readable tab title new-api's front-end shows
// via `groupTiersByPrimaryDim`. Contract §6.2: the parser picks the
// first `resolution=...` fragment as the group; anything else in the
// label is decoration.
func tierLabel(d domain.ModelPriceDecision) string {
	return tierLabelFrom(d.Resolution, d.Audio, d.Mode)
}

func tierLabelFor(r pricingRow) string {
	return tierLabelFrom(r.Resolution, r.Audio, r.Mode)
}

func tierLabelFrom(resolution, audio, mode string) string {
	parts := make([]string, 0, 3)
	if resolution != "" {
		parts = append(parts, resolution)
	}
	if audio != "" {
		parts = append(parts, "audio="+audio)
	}
	if mode != "" {
		parts = append(parts, "mode="+mode)
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, " · ")
}

// dslTier is the intermediate value the DSL builder threads between
// its classify → dedupe → emit passes. Kept file-private since the DSL
// output is the only public surface.
type dslTier struct {
	resolution string
	audio      string
	mode       string
	label      string
	fixed      int64
	note       string
}

// buildGuard assembles the has() chain for one tier. Empty dimensions
// contribute no guard clause — a tier with resolution=720p / audio="" /
// mode="" produces just `has(param("resolution"),"720p")`. When every
// dimension is empty the guard is empty (caller handles as terminal).
func buildGuard(t dslTier) string {
	clauses := make([]string, 0, 3)
	if t.resolution != "" {
		clauses = append(clauses, fmt.Sprintf(`has(param("resolution"),%s)`, quoteNote(t.resolution)))
	}
	if t.audio != "" {
		clauses = append(clauses, fmt.Sprintf(`has(param("audio"),%s)`, quoteNote(t.audio)))
	}
	if t.mode != "" {
		clauses = append(clauses, fmt.Sprintf(`has(param("mode"),%s)`, quoteNote(t.mode)))
	}
	if len(clauses) == 0 {
		return ""
	}
	return strings.Join(clauses, " && ")
}

// fallbackTier is the mandatory terminal tier from contract §6.2:
// "every generated billing_expr ends with tier('unpriced', 0, 'no
// matching variant')" so the DSL evaluator never falls off the end.
// §5 requires downstream to treat the resulting fixedPrice=0 as
// "unmatched" (401/402 or its equivalent), not "free".
func fallbackTier() string {
	return `tier("unpriced", 0, "no matching variant")`
}

// quoteNote wraps a string in double quotes and escapes embedded
// quotes / backslashes. new-api's DSL parser is a small
// hand-written state machine; keeping the escaping minimal
// (\\  and \") avoids parser quirks with unicode escape forms.
func quoteNote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// compareResolution returns <0 if a is a "higher-priority" resolution
// tab than b. Priority is descending numeric height: 4k > 1080p >
// 720p > 480p > everything else alphabetic. Empty strings sort last
// (caller filters those to the terminal tier).
func compareResolution(a, b string) int {
	ra := resolutionRank(a)
	rb := resolutionRank(b)
	if ra != rb {
		// Higher rank first (descending).
		if ra > rb {
			return -1
		}
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

// resolutionRank returns a numeric priority — bigger = higher quality.
// Ranks are hand-picked for the common video resolutions rather than
// parsed from the string; new resolutions get their rank added here.
func resolutionRank(r string) int {
	switch strings.ToLower(r) {
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

// compareAudio returns <0 when a should appear before b. Rule:
// on > off > empty. Rationale: audio-on is the higher-price tier,
// so it should be checked first at match time; empty is the
// resolution-only default and belongs last.
func compareAudio(a, b string) int {
	ra := audioRank(a)
	rb := audioRank(b)
	if ra != rb {
		if ra > rb {
			return -1
		}
		return 1
	}
	return 0
}

func audioRank(a string) int {
	switch a {
	case "on":
		return 3
	case "voice_control":
		return 2
	case "off":
		return 1
	}
	return 0
}

