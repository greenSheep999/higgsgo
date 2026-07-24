package v1

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// TestDownstreamPricing_NewApiParserGolden reproduces new-api's front-end
// parseTiersFromExpr regex + computeDiscountRatio arithmetic against
// higgsgo's emitted /api/pricing feed, end-to-end. The regex on line 40 is
// verbatim from new-api/web/default/src/features/pricing/lib/billing-expr.ts
// (line 310). If this file is edited on either side, the diff MUST be
// coordinated — this is the only place both sides prove agreement.
//
// Wire scenario: kling-3 with two operator overrides.
//   - 720p × off: override PriceMicros=50_000/s → fixed=250_000 total
//     over 5s; raw provider = 84_000/s → official=420_000 total.
//     Expected discount ratio = 250_000/420_000 ≈ 0.5952 (~40% off badge).
//   - 1080p × on : override PriceMicros=200_000/s → fixed=1_000_000;
//     raw = 168_000/s → official=840_000. Ratio = 1.19 (premium, no badge).
//   - 720p × on / 1080p × off: no override, tier note omits
//     official_micros; front-end falls back to model-level.
func TestDownstreamPricing_NewApiParserGolden(t *testing.T) {
	store := &fakeDownstreamPricingStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "K", Unit: "per_second",
				PriceMicros: 84000, Resolution: "720p", Audio: "off", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "K", Unit: "per_second",
				PriceMicros: 126000, Resolution: "720p", Audio: "on", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "K", Unit: "per_second",
				PriceMicros: 112000, Resolution: "1080p", Audio: "off", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "K", Unit: "per_second",
				PriceMicros: 168000, Resolution: "1080p", Audio: "on", DurationSeconds: 5},
		},
		decisions: []domain.ModelPriceDecision{
			{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 50000,
				Resolution: "720p", Audio: "off", DurationSeconds: 5},
			{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 200000,
				Resolution: "1080p", Audio: "on", DurationSeconds: 5},
		},
	}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))

	var body struct {
		Data []struct {
			ModelName           string `json:"model_name"`
			BillingExpr         string `json:"billing_expr"`
			OfficialPriceMicros int64  `json:"official_price_micros"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Data))
	}
	row := body.Data[0]

	// Verbatim mirror of new-api's parseTiersFromExpr tier-matching regex.
	// If new-api changes this, coordinate.
	tierRe := regexp.MustCompile(
		`tier\("([^"]*)",\s*([^,)]+?)(?:,\s*"([^"]*)")?\s*\)`,
	)
	kvRe := regexp.MustCompile(`^([a-zA-Z_][\w]*)\s*=\s*(.+)$`)

	type parsedTier struct {
		label      string
		fixedUSD   float64
		officialUS float64 // per-tier from note, or 0 = fall back to model-level
	}
	tiers := []parsedTier{}
	for _, m := range tierRe.FindAllStringSubmatch(row.BillingExpr, -1) {
		label := m[1]
		bodyStr := strings.TrimSpace(m[2])
		note := m[3]
		fixedMicros, _ := strconv.ParseFloat(bodyStr, 64)
		pt := parsedTier{label: label, fixedUSD: fixedMicros / 1e6}
		// Parse `k=v · k=v` from note (mirrors new-api's parseTierNoteParams).
		for _, part := range strings.Split(note, "·") {
			part = strings.TrimSpace(part)
			if km := kvRe.FindStringSubmatch(part); km != nil {
				if km[1] == "official_micros" {
					if v, err := strconv.ParseFloat(strings.TrimSpace(km[2]), 64); err == nil {
						pt.officialUS = v / 1e6
					}
				}
			}
		}
		tiers = append(tiers, pt)
	}
	// Expected 5 tiers total: 4 real + 1 fallback.
	if len(tiers) != 5 {
		t.Fatalf("expected 5 tier() calls parsed, got %d: %+v", len(tiers), tiers)
	}

	// Assert the two override tiers have per-tier official_micros.
	find := func(label string) *parsedTier {
		for i := range tiers {
			if tiers[i].label == label {
				return &tiers[i]
			}
		}
		return nil
	}
	t720off := find("720p · audio=off")
	if t720off == nil {
		t.Fatal("720p · audio=off tier missing")
	}
	if math.Abs(t720off.officialUS-0.42) > 1e-6 {
		t.Fatalf("720p off tier: official=%v, want 0.42", t720off.officialUS)
	}
	// Discount ratio = fixed / official. 250000/420000 ≈ 0.5952.
	ratio720 := t720off.fixedUSD / t720off.officialUS
	if math.Abs(ratio720-0.5952) > 0.001 {
		t.Fatalf("720p off ratio = %v, want ~0.5952 (40%% off)", ratio720)
	}

	t1080on := find("1080p · audio=on")
	if t1080on == nil {
		t.Fatal("1080p · audio=on tier missing")
	}
	if math.Abs(t1080on.officialUS-0.84) > 1e-6 {
		t.Fatalf("1080p on tier: official=%v, want 0.84", t1080on.officialUS)
	}
	ratio1080 := t1080on.fixedUSD / t1080on.officialUS
	if math.Abs(ratio1080-1.190) > 0.01 {
		t.Fatalf("1080p on ratio = %v, want ~1.19 (premium)", ratio1080)
	}
	if ratio1080 <= 1.0 {
		t.Fatal("premium tier ratio should be > 1 (front-end skips badge)")
	}

	// Assert the two untouched tiers have NO per-tier official_micros.
	t720on := find("720p · audio=on")
	if t720on == nil || t720on.officialUS != 0 {
		t.Fatalf("720p on should have no per-tier official (falls back to model-level); got %+v", t720on)
	}
	t1080off := find("1080p · audio=off")
	if t1080off == nil || t1080off.officialUS != 0 {
		t.Fatalf("1080p off should have no per-tier official; got %+v", t1080off)
	}

	// Model-level official_price_micros is the cheapest tier fold —
	// here 84000 × 5 = 420_000 micros.
	if row.OfficialPriceMicros != 420000 {
		t.Fatalf("model-level official = %d, want 420000", row.OfficialPriceMicros)
	}

	// End-to-end: model-level ratio (fixed / model-level official) for
	// the untouched 720p × on tier. Since fixed matches raw scrape here
	// (126_000 × 5 = 630_000) and model-level = 420_000 (cheapest),
	// ratio = 1.5. new-api would NOT render a discount for this variant,
	// but the calculation must be well-defined.
	if t720on == nil {
		t.Fatal("720p on missing (already checked)")
	}
	modelRatio := t720on.fixedUSD / (float64(row.OfficialPriceMicros) / 1e6)
	if math.Abs(modelRatio-1.5) > 1e-6 {
		t.Fatalf("720p on model-level ratio = %v, want 1.5", modelRatio)
	}
}
