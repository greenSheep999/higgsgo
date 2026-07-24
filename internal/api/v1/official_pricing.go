package v1

import (
	"net/http"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// HandleOfficialAPIPricing serves GET /api/pricing/official-api.
//
// This is the "market-data feed" from contract §6.4 — it publishes the
// raw official / public-API price each model would cost if the customer
// went to the provider directly, unmodified. new-api's model-price-
// adapter is the primary consumer: it uses this to populate the
// "official reference" column of its model catalog. Contract-defined
// shape:
//
//	{
//	  "success": true,
//	  "generated_at": "<RFC3339>",
//	  "data": [
//	    {
//	      "model_name": "<canonical alias>",
//	      "jst":        "<JST>",
//	      "references": [ { provider, resolution, audio, mode, unit,
//	                        duration_seconds, amount_micros, currency,
//	                        source_url, observed_at }, ... ]
//	    }, ...
//	  ]
//	}
//
// Behavior notes locked in with the contract:
//   - Models with zero references are omitted from `data`.
//   - Multiple providers per model are allowed; downstream picks which
//     to display.
//   - Optional `?model=<alias-or-jst>` filters to one row so downstream
//     can spot-check a single model without pulling the full catalog.
//   - Cache-friendly: this endpoint only changes when the operator
//     imports a fresh official_price_observations batch. We hint 6h
//     max-age (contract §6.4's recommended TTL floor) so a well-behaved
//     downstream can cache aggressively.
func (h *Handler) HandleOfficialAPIPricing(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing_store_unavailable",
			"pricing persistence is not configured")
		return
	}
	observations, err := h.Pricing.ListAllOfficialPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}

	// Registry gives us the canonical alias per JST plus any extra
	// aliases. We key the response on `model_name` = canonical alias
	// (matches /v1/pricing's model_aliases[0] semantics); JST is
	// carried alongside so downstream can join with /api/pricing
	// tiers without an extra registry call.
	//
	// Fallback: if a JST isn't in the registry (stale observation, or
	// the model was removed from the catalog), we still surface the
	// row keyed on model_alias verbatim — dropping it would silently
	// hide data the operator ingested on purpose.
	jstByAlias, canonicalByJST := h.officialPricingIndex()

	filter := strings.TrimSpace(r.URL.Query().Get("model"))
	// Resolve the filter to a JST up-front so an extra alias
	// ("kling-v3") matches observations stored under the canonical
	// alias ("kling-3") for the same JST. If the filter isn't in the
	// alias index we fall through to raw string comparison so a JST
	// or an alias unknown to the registry still filters correctly.
	filterJST := ""
	if filter != "" {
		if j, ok := jstByAlias[filter]; ok {
			filterJST = j
		}
	}

	type reference struct {
		Provider        string `json:"provider"`
		Resolution      string `json:"resolution"`
		Audio           string `json:"audio"`
		Mode            string `json:"mode"`
		Unit            string `json:"unit"`
		DurationSeconds int    `json:"duration_seconds"`
		AmountMicros    int64  `json:"amount_micros"`
		Currency        string `json:"currency"`
		SourceURL       string `json:"source_url"`
		ObservedAt      string `json:"observed_at"`
	}
	type modelEntry struct {
		ModelName  string      `json:"model_name"`
		JST        string      `json:"jst"`
		References []reference `json:"references"`
	}

	// One entry per model, order of first appearance in the sorted
	// observations slice (which is `ORDER BY model_alias, ...` — see
	// PricingStore.ListAllOfficialPrices). Grouping is a single pass.
	entries := make([]*modelEntry, 0, 8)
	indexByAlias := make(map[string]int, 8)

	for _, obs := range observations {
		aliasKey := obs.ModelAlias
		if aliasKey == "" {
			continue
		}
		jst := jstByAlias[aliasKey]
		canonical := aliasKey
		if jst != "" {
			if canon := canonicalByJST[jst]; canon != "" {
				canonical = canon
			}
		}

		// Apply the ?model= filter after we know the JST + canonical.
		// Accept either the stored alias, the JST, the canonical alias,
		// or (via jstByAlias lookup up-front) any extra alias mapped
		// to the same JST.
		if filter != "" && !officialPricingMatchesFilter(filter, filterJST, aliasKey, jst, canonical) {
			continue
		}

		idx, ok := indexByAlias[canonical]
		if !ok {
			idx = len(entries)
			entries = append(entries, &modelEntry{
				ModelName:  canonical,
				JST:        jst,
				References: make([]reference, 0, 4),
			})
			indexByAlias[canonical] = idx
		}
		entries[idx].References = append(entries[idx].References, reference{
			Provider:        obs.Provider,
			Resolution:      obs.Resolution,
			Audio:           obs.Audio,
			Mode:            obs.Mode,
			Unit:            obs.Unit,
			DurationSeconds: obs.DurationSeconds,
			AmountMicros:    obs.PriceMicros,
			Currency:        firstNonEmpty(obs.Currency, "USD"),
			SourceURL:       obs.SourceURL,
			ObservedAt:      obs.ObservedAt.UTC().Format(time.RFC3339),
		})
	}

	// Contract §6.4: "Models with zero references are omitted from
	// `data`." Grouping only creates an entry when there's at least
	// one observation, so this is already true — but we make it
	// explicit for anyone reading the response builder.
	data := make([]modelEntry, 0, len(entries))
	for _, e := range entries {
		if len(e.References) == 0 {
			continue
		}
		data = append(data, *e)
	}

	// 6h max-age matches the TTL floor recommended in contract §6.4.
	// The endpoint changes only on operator-driven imports, so a stale
	// window here is at most one import cycle — safe to cache.
	w.Header().Set("Cache-Control", "public, max-age=21600")

	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"data":         data,
	})
}

// officialPricingIndex builds two maps from the model registry:
//   - jstByAlias: every alias (canonical + extras) → JST, so an
//     observation stored under any known alias resolves to its JST.
//   - canonicalByJST: JST → canonical alias, used to normalize the
//     `model_name` field in the response.
//
// Both maps are computed once per request; the registry snapshot is
// already in-memory so this is cheap. Unknown aliases are simply
// absent from the maps — the caller falls back to the observation's
// stored alias verbatim, per the "don't silently drop rows" note in
// HandleOfficialAPIPricing.
func (h *Handler) officialPricingIndex() (jstByAlias, canonicalByJST map[string]string) {
	jstByAlias = make(map[string]string)
	canonicalByJST = make(map[string]string)
	if h.Registry == nil {
		return
	}
	for _, spec := range h.Registry.List(ports.ModelFilter{IncludeUnstable: true, IncludeDeprecated: true}) {
		if spec == nil || spec.JST == "" {
			continue
		}
		if _, ok := canonicalByJST[spec.JST]; !ok {
			canonicalByJST[spec.JST] = spec.Alias
		}
		jstByAlias[spec.Alias] = spec.JST
		for _, alt := range spec.ExtraAliases {
			jstByAlias[alt] = spec.JST
		}
	}
	return
}

// officialPricingMatchesFilter is the accept-check for the ?model= query
// param. It accepts the observation if ANY of the following holds:
//   - filterJST resolved from the alias index equals the observation's JST
//     (matches "kling-v3" against a "kling-3" observation, both share
//     JST "kling3_0")
//   - the raw filter string equals the stored alias, the JST, or the
//     canonical alias, case-insensitive
//
// The two branches are intentionally distinct: filterJST covers the
// registry-known aliasing case, and the string-equality branch handles
// filters the registry doesn't know about (e.g. a JST directly, or a
// stored alias for an "orphan" observation whose model was later
// removed from the catalog).
func officialPricingMatchesFilter(filter, filterJST, aliasKey, jst, canonical string) bool {
	if filter == "" {
		return true
	}
	if filterJST != "" && jst == filterJST {
		return true
	}
	if strings.EqualFold(aliasKey, filter) ||
		strings.EqualFold(jst, filter) ||
		strings.EqualFold(canonical, filter) {
		return true
	}
	return false
}

// firstNonEmpty returns the first non-empty string in the arguments,
// falling back to the empty string if all are empty. Handy for
// currency defaulting where we want "USD" if the observation didn't
// carry a currency but not to overwrite a set value like "CNY".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

