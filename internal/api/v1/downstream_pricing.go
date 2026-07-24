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
// Contract §6.2: the downstream feed new-api's ratio_sync.go consumes.
// One row per Higgs alias that has ≥1 priced variant; each row carries a
// billing_expr DSL string that new-api's parser resolves at request time
// to the tier matching (resolution, audio, mode). Fields:
//
//	model_name   — Higgs public alias
//	quota_type   — always 2 (tiered_expr; no quota_type=1 to avoid the
//	               "per 500K tokens" ambiguity in new-api's model_price)
//	billing_mode — "tiered_expr"
//	billing_expr — DSL, see §6.2 grammar
//	lifecycle    — {"status":"active"} today; §7 will grow deprecation
//	               and sunset transitions
//
// Wire example for kling-3 (excerpt of contract §3.1):
//
//	has(param("resolution"),"1080p") ?
//	  tier("1080p · audio=on", 840000, "per_second · duration_seconds=5") :
//	  has(param("resolution"),"720p") ?
//	    tier("720p · audio=on", 630000, "per_second · duration_seconds=5") :
//	    tier("unpriced", 0, "no matching variant")
//
// Rows with no decisions are OMITTED (contract §3.1). Aliases with zero
// priced variants do not appear in `data`; downstream falls back to its
// own defaults for them.
//
// Auth: mounted under `/api/*` next to `/api/pricing/official-api`, so
// the same sk-hg-* APIKeyAuth applies. This is a trusted-infra endpoint.
func (h *Handler) HandleDownstreamPricing(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing_store_unavailable",
			"pricing persistence is not configured")
		return
	}
	decisions, err := h.Pricing.ListLatestPriceDecisions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}

	// Optional single-alias filter mirrors /v1/pricing and
	// /api/pricing/official-api. Case-insensitive so callers don't
	// have to remember the canonical form.
	filter := strings.TrimSpace(r.URL.Query().Get("model"))
	byAlias := groupDecisionsByAlias(decisions, filter)

	type item struct {
		ModelName   string         `json:"model_name"`
		QuotaType   int            `json:"quota_type"`
		BillingMode string         `json:"billing_mode"`
		BillingExpr string         `json:"billing_expr"`
		Lifecycle   map[string]any `json:"lifecycle,omitempty"`
	}
	data := make([]item, 0, len(byAlias))
	// Deterministic order so downstream diffs are stable across calls.
	aliases := make([]string, 0, len(byAlias))
	for alias := range byAlias {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		aliasDecisions := byAlias[alias]
		expr := buildBillingExpr(aliasDecisions)
		if expr == "" {
			continue
		}
		data = append(data, item{
			ModelName:   alias,
			QuotaType:   2,
			BillingMode: "tiered_expr",
			BillingExpr: expr,
			Lifecycle:   map[string]any{"status": "active"},
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

// groupDecisionsByAlias buckets decisions per alias and applies the
// optional filter. Filter matching mirrors /v1/pricing: exact case-
// insensitive on the model_alias field. JST-based lookup is left off
// intentionally — /api/pricing keys downstream on the public alias so
// letting a caller filter by JST would surface an alias the feed itself
// exposes under a different name.
func groupDecisionsByAlias(decisions []domain.ModelPriceDecision, filter string) map[string][]domain.ModelPriceDecision {
	out := make(map[string][]domain.ModelPriceDecision, 8)
	for _, d := range decisions {
		if d.ModelAlias == "" || d.PriceMicros <= 0 {
			continue
		}
		if filter != "" && !strings.EqualFold(d.ModelAlias, filter) {
			continue
		}
		out[d.ModelAlias] = append(out[d.ModelAlias], d)
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
func buildBillingExpr(decisions []domain.ModelPriceDecision) string {
	if len(decisions) == 0 {
		return ""
	}

	// Pass 1: classify each decision → tier{guards, label, fixedMicros, note}.
	tiers := make([]dslTier, 0, len(decisions))
	for _, d := range decisions {
		fixed := d.PriceMicros
		note := d.Unit
		if d.Unit == "per_second" && d.DurationSeconds > 0 {
			fixed = d.PriceMicros * int64(d.DurationSeconds)
			note = fmt.Sprintf("per_second · duration_seconds=%d", d.DurationSeconds)
		} else if d.DurationSeconds > 0 {
			note = fmt.Sprintf("%s · duration_seconds=%d", d.Unit, d.DurationSeconds)
		}
		tiers = append(tiers, dslTier{
			resolution: d.Resolution,
			audio:      d.Audio,
			mode:       d.Mode,
			label:      tierLabel(d),
			fixed:      fixed,
			note:       note,
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
	parts := make([]string, 0, 3)
	if d.Resolution != "" {
		parts = append(parts, d.Resolution)
	}
	if d.Audio != "" {
		parts = append(parts, "audio="+d.Audio)
	}
	if d.Mode != "" {
		parts = append(parts, "mode="+d.Mode)
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

