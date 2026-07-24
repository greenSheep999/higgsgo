package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestDumpWireResponse writes real /api/pricing + /api/pricing/official-api
// response bytes to /tmp so an operator can hand them to new-api's team
// as a golden fixture without booting the full server. Run with:
//   go test ./internal/api/v1/ -run TestDumpWireResponse -v
// Skipped in CI by requiring HIGGSGO_DUMP_WIRE=1.
func TestDumpWireResponse(t *testing.T) {
	if os.Getenv("HIGGSGO_DUMP_WIRE") != "1" {
		t.Skip("set HIGGSGO_DUMP_WIRE=1 to write /tmp/higgsgo-wire-*.json")
	}

	// Real Kling-3 decisions as we'd emit them once operator approves
	// the intl pricing (contract §3.1 expects sell prices, not raw
	// observations — this endpoint is the billing feed).
	decisions := []domain.ModelPriceDecision{
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 250000, Resolution: "720p", Audio: "off", DurationSeconds: 5},
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 380000, Resolution: "720p", Audio: "on", DurationSeconds: 5},
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 340000, Resolution: "1080p", Audio: "off", DurationSeconds: 5},
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 500000, Resolution: "1080p", Audio: "on", DurationSeconds: 5},
	}
	obs := []domain.OfficialPriceObservation{
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "720p", Audio: "off", PriceMicros: 84000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl"},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "720p", Audio: "on", PriceMicros: 126000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl"},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "1080p", Audio: "off", PriceMicros: 112000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl"},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "1080p", Audio: "on", PriceMicros: 168000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl"},
	}

	h := &Handler{Pricing: &wireStubStore{decisions: decisions, obs: obs}}

	// /api/pricing (downstream billing feed)
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	writeWireFixture(t, "/tmp/higgsgo-wire-api-pricing.json", rec)

	// /api/pricing/official-api (reference market data)
	rec = httptest.NewRecorder()
	h.HandleOfficialAPIPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing/official-api", nil))
	writeWireFixture(t, "/tmp/higgsgo-wire-official-api.json", rec)

	// /api/pricing?model=kling-3 for filter sanity check
	rec = httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing?model=kling-3", nil))
	writeWireFixture(t, "/tmp/higgsgo-wire-api-pricing-kling3.json", rec)
}

func writeWireFixture(t *testing.T, path string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var v any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	fmt.Printf("wrote %s (%d bytes)\n", path, len(out))
}

type wireStubStore struct {
	ports.PricingStore
	decisions []domain.ModelPriceDecision
	obs       []domain.OfficialPriceObservation
}

func (s *wireStubStore) ListLatestPriceDecisions(context.Context) ([]domain.ModelPriceDecision, error) {
	return s.decisions, nil
}
func (s *wireStubStore) ListAllOfficialPrices(context.Context) ([]domain.OfficialPriceObservation, error) {
	return s.obs, nil
}
