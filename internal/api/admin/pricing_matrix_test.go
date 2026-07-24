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

type pricingMatrixRegistry struct{ ports.ModelRegistry }

func (pricingMatrixRegistry) Resolve(alias string) (*domain.ModelSpec, error) {
	if alias != "kling-3" {
		return nil, domain.ErrModelNotFound
	}
	return &domain.ModelSpec{Alias: alias, JST: "kling3_0", MinPlan: domain.PlanStarter}, nil
}

type pricingMatrixStore struct{ ports.PricingStore }

func (pricingMatrixStore) ListLatestRules(context.Context, string) ([]domain.ModelCostRule, error) {
	return []domain.ModelCostRule{{
		JST: "kling3_0", Unit: "per_second", Component: "audio_state",
		CreditsHundredths: 250, Resolution: "720p", Audio: "on",
	}}, nil
}

func (pricingMatrixStore) ListPlanCreditRates(context.Context) ([]domain.PlanCreditRate, error) {
	return []domain.PlanCreditRate{{PlanType: "starter", PlanName: "Starter", UnitCostMicros: 75000}}, nil
}

func (pricingMatrixStore) ListOfficialPrices(context.Context, string) ([]domain.OfficialPriceObservation, error) {
	return []domain.OfficialPriceObservation{{
		ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second",
		PriceMicros: 126000, Resolution: "720p", Audio: "on",
	}}, nil
}

func (pricingMatrixStore) ListPriceDecisions(context.Context, string) ([]domain.ModelPriceDecision, error) {
	return []domain.ModelPriceDecision{{
		ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 160000,
		Resolution: "720p", Audio: "on", Rationale: "40% margin floor",
	}}, nil
}

func TestModelsHandler_PricingMatrix(t *testing.T) {
	h := NewModelsHandler(pricingMatrixRegistry{}, nil)
	h.Pricing = pricingMatrixStore{}
	r := chi.NewRouter()
	h.Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/models/kling-3/pricing-matrix", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ModelAlias string `json:"model_alias"`
		Rows       []struct {
			Higgs       []pricingHiggsValue    `json:"higgs"`
			PlanCosts   []pricingPlanCost      `json:"plan_costs"`
			OfficialAPI []pricingOfficialValue `json:"official_api"`
			FinalPrice  *pricingFinalValue     `json:"final_price"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ModelAlias != "kling-3" || len(body.Rows) != 1 {
		t.Fatalf("unexpected matrix: %+v", body)
	}
	row := body.Rows[0]
	if len(row.Higgs) != 1 || row.Higgs[0].Credits != 2.5 {
		t.Fatalf("higgs values = %+v", row.Higgs)
	}
	if len(row.PlanCosts) != 1 || row.PlanCosts[0].USD != 0.1875 {
		t.Fatalf("plan cost = %+v, want $0.1875", row.PlanCosts)
	}
	if len(row.OfficialAPI) == 0 || row.OfficialAPI[0].USD != 0.126 {
		t.Fatalf("official API = %+v", row.OfficialAPI)
	}
	if row.FinalPrice == nil || row.FinalPrice.USD != 0.16 {
		t.Fatalf("final price = %+v", row.FinalPrice)
	}
}

// klingFanoutStore models the real kling3_0 shape: Higgs /job-sets/costs
// returns credits by mode × audio with resolution unset, while the
// official Kling API prices are per-resolution × audio. The matrix
// should treat resolution as the row tier and fan Higgs credits out
// onto every resolution the official grid knows about.
type klingFanoutStore struct{ ports.PricingStore }

func (klingFanoutStore) ListLatestRules(context.Context, string) ([]domain.ModelCostRule, error) {
	// 4 Higgs rules: 2 modes × 2 audio states, no resolution.
	return []domain.ModelCostRule{
		{JST: "kling3_0", Unit: "per_second", Mode: "pro", Audio: "off", CreditsHundredths: 175},
		{JST: "kling3_0", Unit: "per_second", Mode: "pro", Audio: "on", CreditsHundredths: 250},
		{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "off", CreditsHundredths: 150},
		{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "on", CreditsHundredths: 200},
	}, nil
}

func (klingFanoutStore) ListPlanCreditRates(context.Context) ([]domain.PlanCreditRate, error) {
	return []domain.PlanCreditRate{
		{PlanType: "starter", PlanName: "Starter", UnitCostMicros: 75000},
	}, nil
}

func (klingFanoutStore) ListOfficialPrices(context.Context, string) ([]domain.OfficialPriceObservation, error) {
	// 8 Kling observations: 3 resolutions × up to 3 audio each. The 4k
	// column intentionally has no voice_control price to mirror the
	// real Kling doc gap.
	return []domain.OfficialPriceObservation{
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "720p", Audio: "off", PriceMicros: 84000},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "720p", Audio: "on", PriceMicros: 126000},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "720p", Audio: "voice_control", PriceMicros: 154000},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "1080p", Audio: "off", PriceMicros: 112000},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "1080p", Audio: "on", PriceMicros: 168000},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "1080p", Audio: "voice_control", PriceMicros: 196000},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "4k", Audio: "off", PriceMicros: 420000},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling", Unit: "per_second", Resolution: "4k", Audio: "on", PriceMicros: 420000},
	}, nil
}

func (klingFanoutStore) ListPriceDecisions(context.Context, string) ([]domain.ModelPriceDecision, error) {
	return nil, nil
}

// TestModelsHandler_PricingMatrix_KlingFanout locks in the row shape a
// resolution-tiered model with a resolution-agnostic upstream cost
// source should produce.
//
// Expected:
//   - 8 rows total = # of distinct (resolution, audio) pairs seen in
//     official observations. No standalone "Any resolution" rows.
//   - Rows with an audio Higgs also observed at (off/on) get 2 Higgs
//     entries (pro + std) fanned out onto them.
//   - The voice_control rows (720p and 1080p) have no Higgs entries —
//     Higgs credits table doesn't cover that audio value.
func TestModelsHandler_PricingMatrix_KlingFanout(t *testing.T) {
	h := NewModelsHandler(pricingMatrixRegistry{}, nil)
	h.Pricing = klingFanoutStore{}
	r := chi.NewRouter()
	h.Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/models/kling-3/pricing-matrix", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rows []struct {
			Dimensions  pricingDimensions      `json:"dimensions"`
			Higgs       []pricingHiggsValue    `json:"higgs"`
			PlanCosts   []pricingPlanCost      `json:"plan_costs"`
			OfficialAPI []pricingOfficialValue `json:"official_api"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Rows) != 8 {
		t.Fatalf("expected 8 rows (resolution × audio grid), got %d — %+v",
			len(body.Rows), body.Rows)
	}

	// Verify no leftover "any resolution" (empty) rows survived.
	for _, row := range body.Rows {
		if row.Dimensions.Resolution == "" {
			t.Fatalf("row with empty resolution should not exist after fanout: %+v", row.Dimensions)
		}
	}

	// The 720p audio=off row should have two Higgs entries (pro + std)
	// and one official price of $0.084.
	var target *struct {
		Dimensions  pricingDimensions      `json:"dimensions"`
		Higgs       []pricingHiggsValue    `json:"higgs"`
		PlanCosts   []pricingPlanCost      `json:"plan_costs"`
		OfficialAPI []pricingOfficialValue `json:"official_api"`
	}
	for i := range body.Rows {
		row := &body.Rows[i]
		if row.Dimensions.Resolution == "720p" && row.Dimensions.Audio == "off" {
			target = row
			break
		}
	}
	if target == nil {
		t.Fatal("720p × audio=off row missing")
	}
	if len(target.Higgs) != 2 {
		t.Fatalf("720p/off Higgs entries = %d, want 2 (pro + std); got %+v",
			len(target.Higgs), target.Higgs)
	}
	if len(target.OfficialAPI) == 0 || target.OfficialAPI[0].USD != 0.084 {
		t.Fatalf("720p/off official = %+v, want $0.084", target.OfficialAPI)
	}
	// Plan costs: 2 modes × 1 plan = 2 entries.
	if len(target.PlanCosts) != 2 {
		t.Fatalf("720p/off plan_costs = %d, want 2", len(target.PlanCosts))
	}
	// Verify mode tag propagated as component prefix on Higgs values.
	haveModes := map[string]bool{}
	for _, hv := range target.Higgs {
		haveModes[hv.Component] = true
	}
	if !haveModes["pro"] || !haveModes["std"] {
		t.Fatalf("expected pro + std mode tags in Higgs.component; got %+v", target.Higgs)
	}

	// Voice control rows (720p, 1080p) should have no Higgs entries —
	// upstream costs table doesn't cover that audio value.
	for _, row := range body.Rows {
		if row.Dimensions.Audio == "voice_control" && len(row.Higgs) != 0 {
			t.Fatalf("voice_control row at %s has Higgs entries — Higgs data has no voice_control audio: %+v",
				row.Dimensions.Resolution, row.Higgs)
		}
	}
}
