package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeDownstreamPricingStore stubs both ListLatestPriceDecisions
// (operator overrides) and ListAllOfficialPrices (raw provider prices).
// Existing tests seed `decisions` — post-semantics-flip these are
// treated as overrides, i.e. they still land in the downstream feed
// (override-only path when no observations exist).
type fakeDownstreamPricingStore struct {
	ports.PricingStore
	decisions    []domain.ModelPriceDecision
	observations []domain.OfficialPriceObservation
	err          error
	obsErr       error
}

func (f *fakeDownstreamPricingStore) ListLatestPriceDecisions(context.Context) ([]domain.ModelPriceDecision, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.decisions, nil
}

func (f *fakeDownstreamPricingStore) ListAllOfficialPrices(context.Context) ([]domain.OfficialPriceObservation, error) {
	if f.obsErr != nil {
		return nil, f.obsErr
	}
	return f.observations, nil
}

// TestDownstreamPricing_Unavailable exercises the "store not configured"
// branch so a deploy that skipped Pricing on the Handler returns a
// clean 503 rather than a nil-pointer panic.
func TestDownstreamPricing_Unavailable(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDownstreamPricing_ReadFailure(t *testing.T) {
	h := &Handler{Pricing: &fakeDownstreamPricingStore{err: errors.New("boom")}}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDownstreamPricing_EmptyStore mirrors the /api/pricing/official-api
// contract: no decisions yet → 200 + success:true + data:[]. Downstream
// must be able to distinguish "wired but empty" (200) from "not wired"
// (503) during rollout.
func TestDownstreamPricing_EmptyStore(t *testing.T) {
	h := &Handler{Pricing: &fakeDownstreamPricingStore{}}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
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
	if !body.Success || len(body.Data) != 0 {
		t.Fatalf("body = %+v", body)
	}
}

// TestDownstreamPricing_KlingLikeExpr covers the canonical kling-3
// wire example from contract §3.1: two resolutions × two audio values,
// per_second unit, unrolls into fixed_micros = price × duration_seconds,
// and emits a ternary chain with the audio guard nested inside the
// resolution guard. Exercises the mode-folded path (mode="") too.
func TestDownstreamPricing_KlingLikeExpr(t *testing.T) {
	store := &fakeDownstreamPricingStore{decisions: []domain.ModelPriceDecision{
		// kling-3 720p on: $0.126/s × 5s = 630_000 micros total
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 126000, Resolution: "720p", Audio: "on", DurationSeconds: 5},
		// kling-3 720p off: $0.084/s × 5s = 420_000
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 84000, Resolution: "720p", Audio: "off", DurationSeconds: 5},
		// kling-3 1080p on: $0.168/s × 5s = 840_000
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 168000, Resolution: "1080p", Audio: "on", DurationSeconds: 5},
	}}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ModelName   string `json:"model_name"`
			QuotaType   int    `json:"quota_type"`
			BillingMode string `json:"billing_mode"`
			BillingExpr string `json:"billing_expr"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 model, got %d: %+v", len(body.Data), body.Data)
	}
	item := body.Data[0]
	if item.ModelName != "kling-3" || item.QuotaType != 2 || item.BillingMode != "tiered_expr" {
		t.Fatalf("meta mismatch: %+v", item)
	}
	// Assertions on the expr — we don't lock the entire string
	// because label ordering could change if we tweak the sort key.
	// But the tier() amounts and the terminal fallback MUST match
	// contract §6.2 exactly.
	expr := item.BillingExpr
	if !strings.HasSuffix(expr, `tier("unpriced", 0, "no matching variant")`) {
		t.Fatalf("expr missing terminal fallback: %s", expr)
	}
	// 1080p tier: 840_000 total
	if !strings.Contains(expr, `840000`) {
		t.Fatalf("expr missing 1080p total (840000): %s", expr)
	}
	// 720p on tier: 630_000
	if !strings.Contains(expr, `630000`) {
		t.Fatalf("expr missing 720p on total (630000): %s", expr)
	}
	// 720p off tier: 420_000
	if !strings.Contains(expr, `420000`) {
		t.Fatalf("expr missing 720p off total (420000): %s", expr)
	}
	// Guards must use has(param("resolution"),"…")
	if !strings.Contains(expr, `has(param("resolution"),"1080p")`) ||
		!strings.Contains(expr, `has(param("resolution"),"720p")`) {
		t.Fatalf("expr missing resolution guards: %s", expr)
	}
	// Audio guards for 720p (where both audio branches exist).
	if !strings.Contains(expr, `has(param("audio"),"on")`) ||
		!strings.Contains(expr, `has(param("audio"),"off")`) {
		t.Fatalf("expr missing audio guards: %s", expr)
	}
	// Notes should carry the per_second duration marker.
	if !strings.Contains(expr, `"per_second · duration_seconds=5"`) {
		t.Fatalf("expr missing duration_seconds note: %s", expr)
	}
	// 1080p should be listed BEFORE 720p (higher resolution first).
	pos1080 := strings.Index(expr, `"1080p"`)
	pos720 := strings.Index(expr, `"720p"`)
	if pos1080 < 0 || pos720 < 0 || pos1080 > pos720 {
		t.Fatalf("expected 1080p before 720p in expr: %s", expr)
	}
}

// TestDownstreamPricing_PerRequestSingleTier covers the qwen-audio-tts
// pattern (contract §4.2): a model whose only decision has no
// resolution / audio / mode. The DSL degenerates to an unguarded tier
// followed by the mandatory fallback.
func TestDownstreamPricing_PerRequestSingleTier(t *testing.T) {
	store := &fakeDownstreamPricingStore{decisions: []domain.ModelPriceDecision{
		{ModelAlias: "qwen-audio-tts", Unit: "per_request", PriceMicros: 5000},
	}}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			BillingExpr string `json:"billing_expr"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 model, got %d", len(body.Data))
	}
	expr := body.Data[0].BillingExpr
	// Should be: tier("default", 5000, "per_request") : tier("unpriced", 0, ...)
	if !strings.HasPrefix(expr, `tier("default", 5000, "per_request")`) {
		t.Fatalf("expr should start with default tier: %s", expr)
	}
	if !strings.HasSuffix(expr, `tier("unpriced", 0, "no matching variant")`) {
		t.Fatalf("expr missing fallback: %s", expr)
	}
	// No has() guards.
	if strings.Contains(expr, "has(") {
		t.Fatalf("expr should not contain has() for undimensioned model: %s", expr)
	}
}

// TestDownstreamPricing_ModelFilter proves the ?model= filter is
// case-insensitive alias-only. JST-based lookup is intentionally NOT
// supported: /api/pricing keys downstream on the public alias, so
// filtering by JST would return a row under a different name than
// downstream saw in the previous full pull.
func TestDownstreamPricing_ModelFilter(t *testing.T) {
	store := &fakeDownstreamPricingStore{decisions: []domain.ModelPriceDecision{
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 100000, Resolution: "720p", DurationSeconds: 5},
		{ModelAlias: "seedance-2", Unit: "per_second", PriceMicros: 200000, Resolution: "1080p", DurationSeconds: 5},
	}}
	h := &Handler{Pricing: store}

	cases := []struct{ query, want string }{
		{"kling-3", "kling-3"},
		{"KLING-3", "kling-3"},
		{"seedance-2", "seedance-2"},
		{"nobody", ""}, // miss → empty data
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing?model="+tc.query, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			var body struct {
				Data []struct {
					ModelName string `json:"model_name"`
				} `json:"data"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if tc.want == "" {
				if len(body.Data) != 0 {
					t.Fatalf("expected empty, got %+v", body.Data)
				}
				return
			}
			if len(body.Data) != 1 || body.Data[0].ModelName != tc.want {
				t.Fatalf("data = %+v, want single %q", body.Data, tc.want)
			}
		})
	}
}

// TestDownstreamPricing_ZeroPriceOmitted verifies the guard against
// PriceMicros<=0 in groupDecisionsByAlias — an accidentally-zero price
// (or an "unpriced" placeholder row a migration leaked) must not turn
// into a tier that would collide with the fallback's zero value.
func TestDownstreamPricing_ZeroPriceOmitted(t *testing.T) {
	store := &fakeDownstreamPricingStore{decisions: []domain.ModelPriceDecision{
		{ModelAlias: "leaky", Unit: "per_request", PriceMicros: 0},
	}}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	var body struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 0 {
		t.Fatalf("zero-priced decision must be dropped: %+v", body.Data)
	}
}

// TestDownstreamPricing_QuoteEscaping guards against a rationale-esque
// value with quotes/backslashes creeping into a label or note and
// breaking new-api's DSL parser. We inject a mode with an embedded
// double-quote and ensure the output escapes it as \".
func TestDownstreamPricing_QuoteEscaping(t *testing.T) {
	store := &fakeDownstreamPricingStore{decisions: []domain.ModelPriceDecision{
		{ModelAlias: "weird", Unit: "per_request", PriceMicros: 100, Mode: `pro"tier`},
	}}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	var body struct {
		Data []struct {
			BillingExpr string `json:"billing_expr"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Data))
	}
	expr := body.Data[0].BillingExpr
	// Should contain `\"` inside the mode string, never a bare `"`
	// inside the DSL string literal.
	if !strings.Contains(expr, `pro\"tier`) {
		t.Fatalf("expected escaped quote in mode: %s", expr)
	}
}

// TestDownstreamPricing_DeterministicOrder feeds the same fixture twice
// and asserts identical output. Determinism matters because ratio_sync
// diffs the feed to detect price changes; a shuffled billing_expr would
// force a re-write on every poll.
func TestDownstreamPricing_DeterministicOrder(t *testing.T) {
	fixture := []domain.ModelPriceDecision{
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 100000, Resolution: "720p", Audio: "off", DurationSeconds: 5},
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 150000, Resolution: "1080p", Audio: "on", DurationSeconds: 5},
		{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 120000, Resolution: "720p", Audio: "on", DurationSeconds: 5},
	}
	h := &Handler{Pricing: &fakeDownstreamPricingStore{decisions: fixture}}

	pull := func() string {
		rec := httptest.NewRecorder()
		h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
		var body struct {
			Data []struct {
				BillingExpr string `json:"billing_expr"`
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if len(body.Data) != 1 {
			t.Fatalf("expected 1 row")
		}
		return body.Data[0].BillingExpr
	}
	a, b := pull(), pull()
	if a != b {
		t.Fatalf("expr not deterministic:\nA: %s\nB: %s", a, b)
	}
}

// TestDownstreamPricing_ObservationsOnly locks in the flipped-semantics
// contract: /api/pricing serves raw provider prices from
// official_price_observations when there is no operator override.
// This is the primary path (basellm/models.dev-style aggregator behavior).
func TestDownstreamPricing_ObservationsOnly(t *testing.T) {
	store := &fakeDownstreamPricingStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
				PriceMicros: 84000, Resolution: "720p", Audio: "off", DurationSeconds: 5, Region: "intl"},
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
				PriceMicros: 112000, Resolution: "1080p", Audio: "off", DurationSeconds: 5, Region: "intl"},
		},
	}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ModelName   string `json:"model_name"`
			BillingExpr string `json:"billing_expr"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 1 || body.Data[0].ModelName != "kling-3" {
		t.Fatalf("expected kling-3 row from observations, got %+v", body.Data)
	}
	// 720p × 5s = 420_000; 1080p × 5s = 560_000
	expr := body.Data[0].BillingExpr
	if !strings.Contains(expr, "420000") || !strings.Contains(expr, "560000") {
		t.Fatalf("expr missing observation-derived totals: %s", expr)
	}
}

// TestDownstreamPricing_OverrideBeatsObservation confirms operator
// overrides in model_price_decisions replace the raw observation for
// the same (alias, resolution, audio, mode) tuple. The other tuples
// on the same alias still flow through from observations.
func TestDownstreamPricing_OverrideBeatsObservation(t *testing.T) {
	store := &fakeDownstreamPricingStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
				PriceMicros: 84000, Resolution: "720p", Audio: "off", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
				PriceMicros: 112000, Resolution: "1080p", Audio: "off", DurationSeconds: 5},
		},
		decisions: []domain.ModelPriceDecision{
			// Override just 720p — back-solved to keep retail floor
			{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: 100000,
				Resolution: "720p", Audio: "off", DurationSeconds: 5},
		},
	}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			BillingExpr string `json:"billing_expr"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Data))
	}
	expr := body.Data[0].BillingExpr
	// 720p override: 100000 × 5 = 500_000 is the fixed_micros.
	if !strings.Contains(expr, "500000") {
		t.Fatalf("expected override fixed 500000 in expr: %s", expr)
	}
	// 1080p untouched observation: 112000 × 5 = 560_000 as fixed.
	if !strings.Contains(expr, "560000") {
		t.Fatalf("expected untouched observation 560000 in expr: %s", expr)
	}
	// The 720p override's tier note MUST carry official_micros=420000
	// (raw × duration), so new-api front-end can compute the discount:
	// 500000 / 420000 = 1.19× (i.e. a premium, not a discount).
	if !strings.Contains(expr, "official_micros=420000") {
		t.Fatalf("expected official_micros=420000 in override tier note: %s", expr)
	}
	// The 1080p tier has NO override so its note must NOT carry an
	// official_micros suffix (raw = fixed, no discount to render).
	if strings.Contains(expr, "official_micros=560000") {
		t.Fatalf("unmodified tier should skip official_micros; got: %s", expr)
	}
}

// TestDownstreamPricing_ModelLevelOfficialPrice asserts each model in
// the feed carries an `official_price_micros` at the top level, set to
// the cheapest tier's official (duration-folded) price. This is the
// fallback new-api front-end uses when rendering a single "starts
// from N% off" badge on the model card (see computeModelBestDiscount).
func TestDownstreamPricing_ModelLevelOfficialPrice(t *testing.T) {
	store := &fakeDownstreamPricingStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
				PriceMicros: 84000, Resolution: "720p", Audio: "off", DurationSeconds: 5},
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
				PriceMicros: 168000, Resolution: "1080p", Audio: "on", DurationSeconds: 5},
		},
	}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	var body struct {
		Data []struct {
			ModelName           string `json:"model_name"`
			OfficialPriceMicros int64  `json:"official_price_micros"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Data))
	}
	// cheapest tier = 720p × 5 = 420_000 micros = $0.42/call.
	if body.Data[0].OfficialPriceMicros != 420000 {
		t.Fatalf("model-level official = %d, want 420000 (cheapest tier duration-folded)",
			body.Data[0].OfficialPriceMicros)
	}
}

// TestDownstreamPricing_EstimatedObservationsDropped confirms rows
// flagged estimated=true (from raw-pricing/*-cn.md derivations) are
// not published to the downstream feed. Operator can still see them
// in the internal admin UI, but they don't ship to new-api.
func TestDownstreamPricing_EstimatedObservationsDropped(t *testing.T) {
	store := &fakeDownstreamPricingStore{
		observations: []domain.OfficialPriceObservation{
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (CN)", Unit: "per_second",
				PriceMicros: 600000, Resolution: "720p", Audio: "off", DurationSeconds: 5,
				Region: "cn", Estimated: true}, // ← should be filtered
			{ModelAlias: "kling-3", Provider: "Kuaishou Kling (Intl)", Unit: "per_second",
				PriceMicros: 84000, Resolution: "720p", Audio: "off", DurationSeconds: 5,
				Region: "intl"},
		},
	}
	h := &Handler{Pricing: store}
	rec := httptest.NewRecorder()
	h.HandleDownstreamPricing(rec, httptest.NewRequest(http.MethodGet, "/api/pricing", nil))
	var body struct {
		Data []struct {
			BillingExpr string `json:"billing_expr"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(body.Data))
	}
	expr := body.Data[0].BillingExpr
	// Only the intl (non-estimated) row should be present.
	if !strings.Contains(expr, "420000") {
		t.Fatalf("expected intl 420000 in expr: %s", expr)
	}
	// The CN estimated row (600000×5=3_000_000) must not appear.
	if strings.Contains(expr, "3000000") {
		t.Fatalf("estimated CN row leaked: %s", expr)
	}
}
