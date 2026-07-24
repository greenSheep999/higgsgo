package v1

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/greensheep999/higgsgo/internal/ports"
)

const higgsPricingSource = "higgs_job_set_costs"

// HandlePricingCatalog serves GET /v1/pricing. It exposes the latest lossless
// upstream credit rules; it intentionally does not invent a downstream USD
// sell price before an operator policy is configured.
func (h *Handler) HandlePricingCatalog(w http.ResponseWriter, r *http.Request) {
	if h.Pricing == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing_store_unavailable", "pricing persistence is not configured")
		return
	}
	snapshot, err := h.Pricing.LatestSnapshot(r.Context(), higgsPricingSource)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}
	if snapshot == nil {
		writeError(w, http.StatusServiceUnavailable, "pricing_not_ready", "no upstream pricing snapshot has been collected yet")
		return
	}
	rules, err := h.Pricing.ListLatestRules(r.Context(), higgsPricingSource)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pricing_read_failed", err.Error())
		return
	}

	aliasesByJST := h.pricingAliasesByJST()
	filter := strings.TrimSpace(r.URL.Query().Get("model"))
	items := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		aliases := aliasesByJST[rule.JST]
		if filter != "" && filter != rule.JST && !containsString(aliases, filter) {
			continue
		}
		var dimensions map[string]any
		_ = json.Unmarshal([]byte(rule.DimensionsJSON), &dimensions)
		items = append(items, map[string]any{
			"jst":                         rule.JST,
			"model_aliases":               aliases,
			"unit":                        rule.Unit,
			"component":                   rule.Component,
			"credits":                     float64(rule.CreditsHundredths) / 100,
			"credits_hundredths":          rule.CreditsHundredths,
			"original_credits":            float64(rule.OriginalCreditsHundredths) / 100,
			"original_credits_hundredths": rule.OriginalCreditsHundredths,
			"resolution":                  rule.Resolution,
			"duration_seconds":            rule.DurationSeconds,
			"mode":                        rule.Mode,
			"audio":                       rule.Audio,
			"dimensions":                  dimensions,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object":         "pricing.catalog",
		"source":         snapshot.Source,
		"source_url":     snapshot.SourceURL,
		"snapshot_id":    snapshot.ID,
		"payload_sha256": snapshot.PayloadSHA256,
		"fetched_at":     snapshot.FetchedAt.Unix(),
		"pricing_scope":  "upstream_credits_only",
		"data":           items,
	})
}

func (h *Handler) pricingAliasesByJST() map[string][]string {
	out := make(map[string][]string)
	if h.Registry == nil {
		return out
	}
	for _, spec := range h.Registry.List(ports.ModelFilter{IncludeUnstable: true, IncludeDeprecated: true}) {
		if spec == nil || spec.JST == "" {
			continue
		}
		aliases := append([]string{spec.Alias}, spec.ExtraAliases...)
		out[spec.JST] = append(out[spec.JST], aliases...)
	}
	for jst := range out {
		sort.Strings(out[jst])
		out[jst] = uniqueStrings(out[jst])
	}
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
