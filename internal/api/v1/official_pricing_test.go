package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeOfficialPricingStore is a minimal PricingStore fake that only wires
// up ListAllOfficialPrices. Other methods panic via the embedded nil
// interface, which is fine — the handler MUST NOT call anything else on
// the store.
type fakeOfficialPricingStore struct {
	ports.PricingStore
	observations []domain.OfficialPriceObservation
	err          error
}

func (f *fakeOfficialPricingStore) ListAllOfficialPrices(context.Context) ([]domain.OfficialPriceObservation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.observations, nil
}

// helperRegistry returns a stub registry with two aliases for kling-3
// (`kling-3` canonical + `kling-v3` extra) so filter tests can hit the
// aliasing path. seedance-2 is included so we can prove models with zero
// observations are simply absent from the response.
func helperRegistry() *fakePricingRegistry {
	return &fakePricingRegistry{models: []*domain.ModelSpec{
		{Alias: "kling-3", JST: "kling3_0", ExtraAliases: []string{"kling-v3"}},
		{Alias: "seedance-2", JST: "seedance_2_0"},
	}}
}

// TestOfficialAPIPricing_Unavailable covers the "no store wired" branch
// so a deployment that skipped PricingStore configuration surfaces 503
// with a clear code rather than a nil-pointer panic.
func TestOfficialAPIPricing_Unavailable(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.HandleOfficialAPIPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing/official-api", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error.Type != "pricing_store_unavailable" {
		t.Fatalf("error type = %q", body.Error.Type)
	}
}

// TestOfficialAPIPricing_ReadFailure passes the 500 path so ops sees a
// distinct code from the 503 "not configured" branch above.
func TestOfficialAPIPricing_ReadFailure(t *testing.T) {
	h := &Handler{Pricing: &fakeOfficialPricingStore{err: errors.New("boom")}}
	rec := httptest.NewRecorder()
	h.HandleOfficialAPIPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing/official-api", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestOfficialAPIPricing_GroupsByModel is the happy path: two observations
// for kling-3 (720p on/off) plus one for a JST that isn't in the
// registry. We expect:
//   - two entries in `data`
//   - kling-3 keyed on the canonical alias with both references attached
//   - the unregistered JST surfaces verbatim (contract §6.4: don't
//     silently drop rows the operator ingested)
//   - Cache-Control header carries 6h max-age
func TestOfficialAPIPricing_GroupsByModel(t *testing.T) {
	obsAt := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	h := &Handler{
		Pricing: &fakeOfficialPricingStore{observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kling Official", Currency: "USD", Unit: "per_second",
				PriceMicros: 84000, Resolution: "720p", Audio: "off",
				SourceURL: "https://kling.ai/pricing", ObservedAt: obsAt},
			{ModelAlias: "kling-3", Provider: "Kling Official", Currency: "USD", Unit: "per_second",
				PriceMicros: 126000, Resolution: "720p", Audio: "on",
				SourceURL: "https://kling.ai/pricing", ObservedAt: obsAt},
			{ModelAlias: "orphan-model", Provider: "Other", Currency: "USD", Unit: "per_request",
				PriceMicros: 5000, ObservedAt: obsAt},
		}},
		Registry: helperRegistry(),
	}
	rec := httptest.NewRecorder()
	h.HandleOfficialAPIPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing/official-api", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=21600" {
		t.Fatalf("Cache-Control = %q, want public, max-age=21600", got)
	}

	var body struct {
		Success     bool   `json:"success"`
		GeneratedAt string `json:"generated_at"`
		Data        []struct {
			ModelName  string `json:"model_name"`
			JST        string `json:"jst"`
			References []struct {
				Provider     string `json:"provider"`
				Resolution   string `json:"resolution"`
				Audio        string `json:"audio"`
				AmountMicros int64  `json:"amount_micros"`
				Currency     string `json:"currency"`
				ObservedAt   string `json:"observed_at"`
			} `json:"references"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if !body.Success {
		t.Fatalf("success = false; body=%s", rec.Body.String())
	}
	if len(body.Data) != 2 {
		t.Fatalf("data len = %d, want 2; body=%s", len(body.Data), rec.Body.String())
	}

	// Find kling-3 entry; ordering is by first-appearance in the
	// observations slice, which is kling-3 → orphan-model here.
	kling := body.Data[0]
	if kling.ModelName != "kling-3" || kling.JST != "kling3_0" {
		t.Fatalf("kling entry = %+v", kling)
	}
	if len(kling.References) != 2 {
		t.Fatalf("kling references = %d, want 2", len(kling.References))
	}
	if kling.References[0].AmountMicros != 84000 || kling.References[1].AmountMicros != 126000 {
		t.Fatalf("kling reference amounts = %v", kling.References)
	}
	if kling.References[0].Currency != "USD" {
		t.Fatalf("currency default not applied: %q", kling.References[0].Currency)
	}
	if kling.References[0].ObservedAt != "2026-07-20T00:00:00Z" {
		t.Fatalf("observed_at format = %q", kling.References[0].ObservedAt)
	}

	// Orphan: model_alias verbatim, JST empty (not in registry).
	orphan := body.Data[1]
	if orphan.ModelName != "orphan-model" || orphan.JST != "" {
		t.Fatalf("orphan entry = %+v", orphan)
	}
}

// TestOfficialAPIPricing_ModelFilter exercises the ?model= filter across
// three lookup keys — canonical alias, extra alias, JST — and confirms
// unmatched aliases yield an empty data array (not a 404).
func TestOfficialAPIPricing_ModelFilter(t *testing.T) {
	obsAt := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	store := &fakeOfficialPricingStore{observations: []domain.OfficialPriceObservation{
		{ModelAlias: "kling-3", Provider: "Kling Official", Unit: "per_second", PriceMicros: 84000, ObservedAt: obsAt},
		{ModelAlias: "seedance-2", Provider: "Bytedance", Unit: "per_second", PriceMicros: 60000, ObservedAt: obsAt},
	}}
	h := &Handler{Pricing: store, Registry: helperRegistry()}

	cases := []struct {
		query     string
		wantModel string // "" means expect zero rows
	}{
		{"kling-3", "kling-3"},   // canonical
		{"kling-v3", "kling-3"},  // extra alias
		{"kling3_0", "kling-3"},  // JST
		{"KLING-3", "kling-3"},   // case-insensitive
		{"nobody-home", ""},      // miss → empty
		{"seedance-2", "seedance-2"},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.HandleOfficialAPIPricing(rec, httptest.NewRequest(http.MethodGet,
				"/api/pricing/official-api?model="+tc.query, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Data []struct {
					ModelName string `json:"model_name"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if tc.wantModel == "" {
				if len(body.Data) != 0 {
					t.Fatalf("expected empty data, got %+v", body.Data)
				}
				return
			}
			if len(body.Data) != 1 || body.Data[0].ModelName != tc.wantModel {
				t.Fatalf("data = %+v, want single %q", body.Data, tc.wantModel)
			}
		})
	}
}

// TestOfficialAPIPricing_EmptyStore confirms the endpoint returns 200 +
// success:true + data:[] when the store has nothing yet, rather than
// 503. Downstream can differentiate "not wired" (503) from "wired,
// nothing imported yet" (200 empty) — important during rollout when
// the endpoint is live before official_price_observations has data.
func TestOfficialAPIPricing_EmptyStore(t *testing.T) {
	h := &Handler{Pricing: &fakeOfficialPricingStore{}, Registry: helperRegistry()}
	rec := httptest.NewRecorder()
	h.HandleOfficialAPIPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing/official-api", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Success bool             `json:"success"`
		Data    []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Success {
		t.Fatalf("success = false on empty store")
	}
	if len(body.Data) != 0 {
		t.Fatalf("data = %+v, want empty", body.Data)
	}
}
