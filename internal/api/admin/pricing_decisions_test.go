package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// recordingPricingStore captures the argument passed to RecordPriceDecision
// so tests can assert the wire→domain mapping. Rules are pre-seeded so the
// retail-floor warning path can be exercised without a full store; every
// other method is inherited from the embedded interface and panics if
// invoked.
type recordingPricingStore struct {
	ports.PricingStore
	saved   []domain.ModelPriceDecision
	rules   []domain.ModelCostRule
	batches []domain.PurchaseBatch
}

func (s *recordingPricingStore) RecordPriceDecision(_ context.Context, d domain.ModelPriceDecision) (domain.ModelPriceDecision, error) {
	d.ID = "prc_stub"
	// Timestamp is derived server-side; leave DecidedAt zero here so the
	// response check can assert the handler filled it.
	s.saved = append(s.saved, d)
	return d, nil
}

func (s *recordingPricingStore) ListLatestRules(_ context.Context, _ string) ([]domain.ModelCostRule, error) {
	return s.rules, nil
}

// ListPurchaseBatches stub — the retail-floor warning path in phase-2
// reads batches to compute the effective unit cost. Returning nil here
// (no batches) exercises the fallback-to-config path deliberately.
// Individual tests that need dynamic pricing seed s.batches and use
// the getter below.
func (s *recordingPricingStore) ListPurchaseBatches(_ context.Context) ([]domain.PurchaseBatch, error) {
	return s.batches, nil
}

func newDecisionsHandler(store *recordingPricingStore) (*ModelsHandler, chi.Router) {
	h := NewModelsHandler(pricingMatrixRegistry{}, nil)
	h.Pricing = store
	r := chi.NewRouter()
	h.Register(r)
	return h, r
}

func TestCreatePricingDecision_HappyPath(t *testing.T) {
	store := &recordingPricingStore{}
	_, r := newDecisionsHandler(store)

	body := bytes.NewBufferString(`{
		"currency": "USD",
		"unit": "per_second",
		"price_micros": 150000,
		"resolution": "720p",
		"mode": "standard",
		"audio": "off",
		"rationale": "60% margin over Kling official"
	}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/models/kling-3/pricing-decisions", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.saved) != 1 {
		t.Fatalf("expected 1 stored decision, got %d", len(store.saved))
	}
	got := store.saved[0]
	if got.ModelAlias != "kling-3" || got.Unit != "per_second" || got.PriceMicros != 150000 {
		t.Fatalf("stored decision mismatch: %+v", got)
	}
	if got.Rationale != "60% margin over Kling official" {
		t.Fatalf("rationale = %q", got.Rationale)
	}

	var resp pricingDecisionView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.USD != 0.15 {
		t.Fatalf("USD echo = %v, want 0.15", resp.USD)
	}
	if resp.ID != "prc_stub" {
		t.Fatalf("ID echo = %q", resp.ID)
	}
}

func TestCreatePricingDecision_UnknownAlias(t *testing.T) {
	store := &recordingPricingStore{}
	_, r := newDecisionsHandler(store)

	body := bytes.NewBufferString(`{"unit":"per_second","price_micros":100000}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/models/does-not-exist/pricing-decisions", body))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if len(store.saved) != 0 {
		t.Fatalf("store must not have been called for unknown alias")
	}
}

func TestCreatePricingDecision_ValidationFailures(t *testing.T) {
	store := &recordingPricingStore{}
	_, r := newDecisionsHandler(store)

	cases := []struct {
		name string
		body string
		code int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"invalid json", "not json", http.StatusBadRequest},
		{"missing unit", `{"price_micros":100000}`, http.StatusBadRequest},
		{"negative price", `{"unit":"per_second","price_micros":-1}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost,
				"/models/kling-3/pricing-decisions", bytes.NewBufferString(tc.body))
			r.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.code, rec.Body.String())
			}
		})
	}
	if len(store.saved) != 0 {
		t.Fatalf("store must stay empty across validation cases, got %d rows", len(store.saved))
	}
}

func TestCreatePricingDecision_UnavailableWhenPricingNil(t *testing.T) {
	h := NewModelsHandler(pricingMatrixRegistry{}, nil)
	// h.Pricing intentionally left nil.
	r := chi.NewRouter()
	h.Register(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/models/kling-3/pricing-decisions",
		bytes.NewBufferString(`{"unit":"per_second","price_micros":100}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreatePricingDecision_RetailBelowFloorWarning verifies contract §10:
// a retail below `credits × unit_cost × markup` writes the row AND attaches
// a retail_below_floor warning. Fixture: kling3_0 std/off is 1.50 credits
// per second (CreditsHundredths=150 = credits × 100); at unit=27_500
// micros/credit and markup=1.8, the floor is
// 150 × 27_500 × 1.8 / 100 = 74_250 micros/sec (~$0.0743). We POST
// $0.05/sec (50_000 micros) and expect the warning.
func TestCreatePricingDecision_RetailBelowFloorWarning(t *testing.T) {
	store := &recordingPricingStore{rules: []domain.ModelCostRule{
		{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "off", CreditsHundredths: 150},
	}}
	h, r := newDecisionsHandler(store)
	h.FloorReferenceUnitCostMicros = 27_500
	h.FloorMarkupMultiplier = 1.8

	body := bytes.NewBufferString(`{
		"unit": "per_second",
		"price_micros": 50000,
		"resolution": "720p",
		"audio": "off"
	}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/models/kling-3/pricing-decisions", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp pricingDecisionView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %+v", len(resp.Warnings), resp.Warnings)
	}
	w := resp.Warnings[0]
	if w.Code != "retail_below_floor" {
		t.Fatalf("code = %q, want retail_below_floor", w.Code)
	}
	// 150 (hundredths = 1.5 credits) × 27500 × 1.8 / 100 = 74_250
	if w.FloorMicros != 74_250 {
		t.Fatalf("floor_micros = %d, want 74250", w.FloorMicros)
	}
	if w.RetailMicros != 50_000 {
		t.Fatalf("retail_micros = %d, want 50000", w.RetailMicros)
	}
	// Row still written despite warning.
	if len(store.saved) != 1 {
		t.Fatalf("row must still write on warning; saved=%d", len(store.saved))
	}
}

// TestCreatePricingDecision_AtOrAboveFloorNoWarning covers the recommended
// branch: retail ≥ floor emits an empty warnings array (omitted on the
// wire) so the WebUI shows nothing but the confirmation. Floor for the
// std/off variant is 74_250 micros/sec; retail 100_000 sits above it.
func TestCreatePricingDecision_AtOrAboveFloorNoWarning(t *testing.T) {
	store := &recordingPricingStore{rules: []domain.ModelCostRule{
		{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "off", CreditsHundredths: 150},
	}}
	h, r := newDecisionsHandler(store)
	h.FloorReferenceUnitCostMicros = 27_500
	h.FloorMarkupMultiplier = 1.8

	body := bytes.NewBufferString(`{
		"unit": "per_second",
		"price_micros": 100000,
		"resolution": "720p",
		"audio": "off"
	}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/models/kling-3/pricing-decisions", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp pricingDecisionView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %+v", resp.Warnings)
	}
}

// TestCreatePricingDecision_CostRuleMissingWarning verifies the graceful
// degradation path: no matching cost rule → the row still writes but the
// operator sees a cost_rule_missing warning explaining why the floor
// check was skipped. Fixture: rules seeded for kling3_0 std but the
// decision targets audio=voice_control which is not in the seed.
func TestCreatePricingDecision_CostRuleMissingWarning(t *testing.T) {
	store := &recordingPricingStore{rules: []domain.ModelCostRule{
		{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "off", CreditsHundredths: 150},
	}}
	h, r := newDecisionsHandler(store)
	h.FloorReferenceUnitCostMicros = 27_500
	h.FloorMarkupMultiplier = 1.8

	body := bytes.NewBufferString(`{
		"unit": "per_second",
		"price_micros": 100000,
		"resolution": "720p",
		"audio": "voice_control"
	}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/models/kling-3/pricing-decisions", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp pricingDecisionView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Warnings) != 1 || resp.Warnings[0].Code != "cost_rule_missing" {
		t.Fatalf("expected 1 cost_rule_missing warning, got %+v", resp.Warnings)
	}
	if len(store.saved) != 1 {
		t.Fatalf("row must still write; saved=%d", len(store.saved))
	}
}

// TestCreatePricingDecision_FloorDisabledWhenConfigZero verifies that
// leaving FloorReferenceUnitCostMicros / FloorMarkupMultiplier at zero
// (per PricingConfig doc "Values ≤ 0 disable the warning entirely")
// skips the check so a naive deployment doesn't get unwanted warnings.
func TestCreatePricingDecision_FloorDisabledWhenConfigZero(t *testing.T) {
	store := &recordingPricingStore{rules: []domain.ModelCostRule{
		{JST: "kling3_0", Unit: "per_second", Mode: "std", Audio: "off", CreditsHundredths: 150},
	}}
	_, r := newDecisionsHandler(store) // FloorReferenceUnitCostMicros left 0

	body := bytes.NewBufferString(`{
		"unit": "per_second",
		"price_micros": 1,
		"resolution": "720p",
		"audio": "off"
	}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/models/kling-3/pricing-decisions", body))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp pricingDecisionView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("expected no warnings when floor disabled, got %+v", resp.Warnings)
	}
}
