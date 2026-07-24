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

// fakeDownstreamPricingStore is a tight fake that only exposes
// ListLatestPriceDecisions. HandleDownstreamPricing MUST NOT reach for
// anything else on the store — the embedded nil interface will panic
// if it does, which is the desired signal in tests.
type fakeDownstreamPricingStore struct {
	ports.PricingStore
	decisions []domain.ModelPriceDecision
	err       error
}

func (f *fakeDownstreamPricingStore) ListLatestPriceDecisions(context.Context) ([]domain.ModelPriceDecision, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.decisions, nil
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
