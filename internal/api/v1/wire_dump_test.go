package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

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

	// No overrides in the baseline fixture: observations flow through
	// unchanged. Populate `decisions` when we want to show an operator
	// back-solve replacing a specific tuple.
	decisions := []domain.ModelPriceDecision{}
	// Fixed observed_at so the emitted /tmp fixture is diff-stable and
	// matches what migration 029 seeds. Real production data comes from
	// operator scrapes / imports with real timestamps; never zero.
	scrapedAt := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	obs := []domain.OfficialPriceObservation{
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "720p", Audio: "off", PriceMicros: 84000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl", ObservedAt: scrapedAt},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "720p", Audio: "on", PriceMicros: 126000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl", ObservedAt: scrapedAt},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "1080p", Audio: "off", PriceMicros: 112000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl", ObservedAt: scrapedAt},
		{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
			Resolution: "1080p", Audio: "on", PriceMicros: 168000, Currency: "USD",
			SourceURL: "https://kling.ai/dev/pricing", Region: "intl", ObservedAt: scrapedAt},
	}

	h := &Handler{Pricing: &wireStubStore{decisions: decisions, obs: obs}}

	// /api/pricing serves the aggregated provider-official prices
	// (post-flip semantics). new-api's ratio_sync.go consumes this
	// exactly like the basellm / models.dev presets — the numbers land
	// in the operator's model_price field, group_ratio applies later.
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	writeWireFixture(t, "/tmp/higgsgo-wire-api-pricing.json", rec)

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
