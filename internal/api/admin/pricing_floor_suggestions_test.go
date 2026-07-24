package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// floorSuggestionsStore is a fixture-driven fake covering the three
// PricingStore methods the endpoint reads. Every other method panics
// via the embedded nil interface — a stronger signal than "returns nil"
// that the handler wandered off spec.
type floorSuggestionsStore struct {
	ports.PricingStore
	rules        []domain.ModelCostRule
	observations []domain.OfficialPriceObservation
	decisions    []domain.ModelPriceDecision
	batches      []domain.PurchaseBatch
}

func (s *floorSuggestionsStore) ListLatestRules(_ context.Context, source string) ([]domain.ModelCostRule, error) {
	if source != "higgs_job_set_costs" {
		return nil, nil
	}
	return s.rules, nil
}

func (s *floorSuggestionsStore) ListAllOfficialPrices(_ context.Context) ([]domain.OfficialPriceObservation, error) {
	return s.observations, nil
}

func (s *floorSuggestionsStore) ListLatestPriceDecisions(_ context.Context) ([]domain.ModelPriceDecision, error) {
	return s.decisions, nil
}

// ListPurchaseBatches stub — phase-2 effective unit cost reads from
// batches. Empty by default so existing floor-suggestions tests keep
// exercising the config-fallback path.
func (s *floorSuggestionsStore) ListPurchaseBatches(_ context.Context) ([]domain.PurchaseBatch, error) {
	return s.batches, nil
}

// floorSuggestionsRegistry stubs enough of ModelRegistry to answer
// aliasToJSTMap. Two aliases point at kling3_0 so the endpoint can
// prove ExtraAliases resolve.
type floorSuggestionsRegistry struct {
	ports.ModelRegistry
}

func (floorSuggestionsRegistry) List(ports.ModelFilter) []*domain.ModelSpec {
	return []*domain.ModelSpec{
		{Alias: "kling-3", JST: "kling3_0", ExtraAliases: []string{"kling-v3"}},
		{Alias: "seedance-2-0", JST: "seedance_2_0"},
	}
}

func mountFloorSuggestionsHandler(h *ModelsHandler) *chi.Mux {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

// TestFloorSuggestions_Unavailable proves the endpoint 503s when the
// Pricing store isn't wired. Distinguishing 503 (misconfigured) from
// 500 (transient) matters during rollout.
func TestFloorSuggestions_Unavailable(t *testing.T) {
	h := NewModelsHandler(floorSuggestionsRegistry{}, nil)
	// h.Pricing left nil.
	r := mountFloorSuggestionsHandler(h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/floor-suggestions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
}

// TestFloorSuggestions_FloorMath verifies the core §10 formula:
//
//	credits × reference_unit_cost × markup / 100
//
// Fixture: kling3_0 std/off at 1.50 credits/sec (CreditsHundredths=150).
// With unit_cost=27_500 and markup=1.8:
//
//	floor = 150 × 27_500 × 1.8 / 100 = 74_250 micros/sec
//
// Plus a matching official observation at 84_000 micros/sec so we can
// verify recommended = max(74_250, 84_000) = 84_000 (median = only
// observation).
func TestFloorSuggestions_FloorMath(t *testing.T) {
	store := &floorSuggestionsStore{
		rules: []domain.ModelCostRule{
			{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "off", CreditsHundredths: 150, DurationSeconds: 5},
		},
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", PriceMicros: 84000,
				Resolution: "720p", Mode: "std", Audio: "off", DurationSeconds: 5},
		},
	}
	h := NewModelsHandler(floorSuggestionsRegistry{}, nil)
	h.Pricing = store
	h.FloorReferenceUnitCostMicros = 27_500
	h.FloorMarkupMultiplier = 1.8

	r := mountFloorSuggestionsHandler(h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/floor-suggestions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Config map[string]any       `json:"config"`
		Rows   []floorSuggestionRow `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Config["enabled"] != true {
		t.Fatalf("config.enabled = %v, want true", body.Config["enabled"])
	}
	if len(body.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(body.Rows))
	}
	row := body.Rows[0]
	if row.ModelAlias != "kling-3" || row.JST != "kling3_0" {
		t.Fatalf("row identity = %+v", row)
	}
	if row.FloorMicros == nil || *row.FloorMicros != 74_250 {
		t.Fatalf("floor = %v, want 74250", row.FloorMicros)
	}
	if row.OfficialMidMicros == nil || *row.OfficialMidMicros != 84_000 {
		t.Fatalf("official mid = %v, want 84000", row.OfficialMidMicros)
	}
	// max(74250, 84000) = 84000
	if row.RecommendedMicros == nil || *row.RecommendedMicros != 84_000 {
		t.Fatalf("recommended = %v, want 84000", row.RecommendedMicros)
	}
	if row.CurrentMicros != nil {
		t.Fatalf("current should be nil (no decision), got %v", row.CurrentMicros)
	}
	if row.CurrentVsFloor != "unknown" {
		t.Fatalf("current_vs_floor = %q, want unknown", row.CurrentVsFloor)
	}
}

// TestFloorSuggestions_FloorDisabled verifies that config with
// reference_unit_cost=0 or markup=0 skips the floor and reports
// floor_reason="floor_disabled" (no panic, no accidental "0" floor).
func TestFloorSuggestions_FloorDisabled(t *testing.T) {
	store := &floorSuggestionsStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "X", Unit: "per_second", PriceMicros: 100000, Resolution: "720p", DurationSeconds: 5},
		},
	}
	h := NewModelsHandler(floorSuggestionsRegistry{}, nil)
	h.Pricing = store
	// FloorReferenceUnitCostMicros left at 0 → disabled.

	r := mountFloorSuggestionsHandler(h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/floor-suggestions", nil))
	var body struct {
		Config map[string]any       `json:"config"`
		Rows   []floorSuggestionRow `json:"rows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Config["enabled"] != false {
		t.Fatalf("config.enabled = %v, want false", body.Config["enabled"])
	}
	if len(body.Rows) != 1 {
		t.Fatalf("rows = %d", len(body.Rows))
	}
	if body.Rows[0].FloorMicros != nil {
		t.Fatalf("floor should be nil when disabled")
	}
	if body.Rows[0].FloorReason != "floor_disabled" {
		t.Fatalf("floor_reason = %q, want floor_disabled", body.Rows[0].FloorReason)
	}
	// Recommended falls back to official mid.
	if body.Rows[0].RecommendedMicros == nil || *body.Rows[0].RecommendedMicros != 100000 {
		t.Fatalf("recommended = %v, want 100000", body.Rows[0].RecommendedMicros)
	}
}

// TestFloorSuggestions_CostRuleMissing exercises the second reason:
// config enabled but no matching rule for the variant → floor nil,
// floor_reason="cost_rule_missing".
func TestFloorSuggestions_CostRuleMissing(t *testing.T) {
	store := &floorSuggestionsStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "seedance-2-0", Provider: "Bytedance", Unit: "per_second", PriceMicros: 500000, Resolution: "720p", DurationSeconds: 5},
		},
	}
	h := NewModelsHandler(floorSuggestionsRegistry{}, nil)
	h.Pricing = store
	h.FloorReferenceUnitCostMicros = 27_500
	h.FloorMarkupMultiplier = 1.8

	r := mountFloorSuggestionsHandler(h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/floor-suggestions", nil))
	var body struct {
		Rows []floorSuggestionRow `json:"rows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Rows) != 1 {
		t.Fatalf("rows = %d", len(body.Rows))
	}
	if body.Rows[0].FloorReason != "cost_rule_missing" {
		t.Fatalf("floor_reason = %q, want cost_rule_missing", body.Rows[0].FloorReason)
	}
	// Recommended still returns something because official_mid is set.
	if body.Rows[0].RecommendedMicros == nil {
		t.Fatalf("recommended should still be set when only official is available")
	}
}

// TestFloorSuggestions_ProviderAggregation feeds three observations
// for the SAME variant tuple across three providers. Low/mid/high
// must reflect min/median/max of the provider distribution, and
// `providers` must list all three sorted.
func TestFloorSuggestions_ProviderAggregation(t *testing.T) {
	store := &floorSuggestionsStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", PriceMicros: 112000, Resolution: "720p", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "fal.ai", Unit: "per_second", PriceMicros: 84000, Resolution: "720p", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "PiAPI", Unit: "per_second", PriceMicros: 168000, Resolution: "720p", DurationSeconds: 5},
		},
	}
	h := NewModelsHandler(floorSuggestionsRegistry{}, nil)
	h.Pricing = store

	r := mountFloorSuggestionsHandler(h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/floor-suggestions", nil))
	var body struct {
		Rows []floorSuggestionRow `json:"rows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(body.Rows))
	}
	row := body.Rows[0]
	if *row.OfficialLowMicros != 84000 || *row.OfficialMidMicros != 112000 || *row.OfficialHighMicros != 168000 {
		t.Fatalf("low/mid/high = %v/%v/%v, want 84000/112000/168000",
			*row.OfficialLowMicros, *row.OfficialMidMicros, *row.OfficialHighMicros)
	}
	if len(row.Providers) != 3 || row.Providers[0] != "Kuaishou Kling" {
		t.Fatalf("providers = %+v, want 3 sorted", row.Providers)
	}
	// Sorted alphabetically: Kuaishou Kling, PiAPI, fal.ai (lowercase after uppercase).
	if row.Providers[0] != "Kuaishou Kling" || row.Providers[1] != "PiAPI" || row.Providers[2] != "fal.ai" {
		t.Fatalf("provider order = %+v, want [Kuaishou Kling, PiAPI, fal.ai]", row.Providers)
	}
}

// TestFloorSuggestions_CurrentAboveBelowAt drives the three
// comparison strings the UI shows in the "current vs floor / official"
// columns. Two variants: one where current sits above floor, one where
// it's exactly at, one below.
func TestFloorSuggestions_CurrentAboveBelowAt(t *testing.T) {
	store := &floorSuggestionsStore{
		rules: []domain.ModelCostRule{
			{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "off", CreditsHundredths: 150, DurationSeconds: 5},
			{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "on", CreditsHundredths: 200, DurationSeconds: 5},
			{JST: "kling3_0", Unit: "per_second", Mode: "pro", Audio: "off", CreditsHundredths: 175, DurationSeconds: 5},
		},
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kuaishou", Unit: "per_second", PriceMicros: 84000, Resolution: "720p", Mode: "std", Audio: "off", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "Kuaishou", Unit: "per_second", PriceMicros: 126000, Resolution: "720p", Mode: "std", Audio: "on", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "Kuaishou", Unit: "per_second", PriceMicros: 112000, Resolution: "1080p", Mode: "pro", Audio: "off", DurationSeconds: 5},
		},
		decisions: []domain.ModelPriceDecision{
			{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 200000, Resolution: "720p", Mode: "std", Audio: "off", DurationSeconds: 5}, // above both
			{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 126000, Resolution: "720p", Mode: "std", Audio: "on", DurationSeconds: 5}, // at official
			{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 50000, Resolution: "1080p", Mode: "pro", Audio: "off", DurationSeconds: 5}, // below both
		},
	}
	h := NewModelsHandler(floorSuggestionsRegistry{}, nil)
	h.Pricing = store
	h.FloorReferenceUnitCostMicros = 27_500
	h.FloorMarkupMultiplier = 1.8

	r := mountFloorSuggestionsHandler(h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/floor-suggestions", nil))
	var body struct {
		Rows []floorSuggestionRow `json:"rows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(body.Rows))
	}
	// Rows sort by resolution DESC (1080p first, then 720p), audio asc within.
	// Row 0: 1080p pro/off  → current=50000, floor=175×27500×1.8/100=86625, official=112000 → below/below
	// Row 1: 720p std/off  → current=200000, floor=74250, official=84000 → above/above
	// Row 2: 720p std/on   → current=126000, floor=99000, official=126000 → above/at
	assertCase := func(idx int, wantVsFloor, wantVsOfficial string) {
		t.Helper()
		r := body.Rows[idx]
		if r.CurrentVsFloor != wantVsFloor {
			t.Fatalf("row[%d] current_vs_floor = %q, want %q (row=%+v)", idx, r.CurrentVsFloor, wantVsFloor, r)
		}
		if r.CurrentVsOfficial != wantVsOfficial {
			t.Fatalf("row[%d] current_vs_official = %q, want %q (row=%+v)", idx, r.CurrentVsOfficial, wantVsOfficial, r)
		}
	}
	assertCase(0, "below", "below")
	assertCase(1, "above", "above")
	assertCase(2, "above", "at")
}

// TestFloorSuggestions_DecisionWithoutObservation covers the case a
// pricing operator adds ahead of the market-data feed: a decision
// exists but there's no matching row in official_price_observations.
// The row must still surface (operator needs to see their own choice),
// with official_* nil.
func TestFloorSuggestions_DecisionWithoutObservation(t *testing.T) {
	store := &floorSuggestionsStore{
		decisions: []domain.ModelPriceDecision{
			{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 150000, Resolution: "720p", DurationSeconds: 5},
		},
	}
	h := NewModelsHandler(floorSuggestionsRegistry{}, nil)
	h.Pricing = store

	r := mountFloorSuggestionsHandler(h)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/floor-suggestions", nil))
	var body struct {
		Rows []floorSuggestionRow `json:"rows"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Rows) != 1 {
		t.Fatalf("rows = %d", len(body.Rows))
	}
	row := body.Rows[0]
	if row.CurrentMicros == nil || *row.CurrentMicros != 150000 {
		t.Fatalf("current = %v", row.CurrentMicros)
	}
	if row.OfficialMidMicros != nil {
		t.Fatalf("official should be nil, got %v", row.OfficialMidMicros)
	}
	if row.CurrentVsOfficial != "unknown" {
		t.Fatalf("current_vs_official = %q, want unknown", row.CurrentVsOfficial)
	}
}
