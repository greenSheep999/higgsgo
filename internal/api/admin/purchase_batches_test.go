package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// purchaseBatchesStore is a fixture-driven fake covering the four
// purchase_batches methods the CRUD endpoint uses. Rows live in an
// in-memory slice so tests can assert on final state after an upsert
// or delete.
type purchaseBatchesStore struct {
	ports.PricingStore
	rows []domain.PurchaseBatch
}

func (s *purchaseBatchesStore) ListPurchaseBatches(_ context.Context) ([]domain.PurchaseBatch, error) {
	return append([]domain.PurchaseBatch(nil), s.rows...), nil
}

func (s *purchaseBatchesStore) GetPurchaseBatch(_ context.Context, id string) (*domain.PurchaseBatch, error) {
	for i := range s.rows {
		if s.rows[i].ID == id {
			r := s.rows[i]
			return &r, nil
		}
	}
	return nil, nil
}

func (s *purchaseBatchesStore) UpsertPurchaseBatch(_ context.Context, b *domain.PurchaseBatch) error {
	for i := range s.rows {
		if s.rows[i].ID == b.ID {
			s.rows[i] = *b
			return nil
		}
	}
	s.rows = append(s.rows, *b)
	return nil
}

func (s *purchaseBatchesStore) DeletePurchaseBatch(_ context.Context, id string) error {
	filtered := s.rows[:0]
	for _, r := range s.rows {
		if r.ID != id {
			filtered = append(filtered, r)
		}
	}
	s.rows = filtered
	return nil
}

func newPurchaseBatchesHandler(store *purchaseBatchesStore) chi.Router {
	h := NewModelsHandler(pricingMatrixRegistry{}, nil)
	h.Pricing = store
	h.FloorReferenceUnitCostMicros = 27_500
	h.FloorMarkupMultiplier = 1.8
	r := chi.NewRouter()
	h.Register(r)
	return r
}

// TestPurchaseBatches_ListEmpty proves the summary comes through even
// on an empty store. Effective unit cost falls back to the config
// value and `fallback_applied` flags it for the UI.
func TestPurchaseBatches_ListEmpty(t *testing.T) {
	r := newPurchaseBatchesHandler(&purchaseBatchesStore{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/purchase-batches", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data    []PurchaseBatchView `json:"data"`
		Summary map[string]any      `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 0 {
		t.Fatalf("data should be empty, got %d rows", len(body.Data))
	}
	if body.Summary["fallback_applied"] != true {
		t.Fatalf("fallback_applied should be true on empty store")
	}
	if body.Summary["effective_unit_cost_micros"] != float64(27_500) {
		t.Fatalf("effective_unit_cost = %v, want 27500", body.Summary["effective_unit_cost_micros"])
	}
}

// TestPurchaseBatches_CreateAndList exercises the round-trip: POST a
// batch with USD inputs, then GET and confirm the row shows up with
// correctly-converted micros AND the summary now reports the
// batch-derived unit cost instead of the fallback.
func TestPurchaseBatches_CreateAndList(t *testing.T) {
	store := &purchaseBatchesStore{}
	r := newPurchaseBatchesHandler(store)

	body := bytes.NewBufferString(`{
		"source_channel": "tg",
		"source_seller": "BLACKHATWORLD",
		"plan_type": "starter",
		"accounts_count": 1,
		"credits_per_account": 200,
		"total_paid_usd": 5.61,
		"linked_account_email": "test@example.com"
	}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pricing/purchase-batches", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created PurchaseBatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	// USD 5.61 → 5_610_000 micros; 200 credits → 20_000 hundredths.
	if created.TotalPaidMicros != 5_610_000 {
		t.Fatalf("total_paid_micros = %d, want 5_610_000", created.TotalPaidMicros)
	}
	if created.CreditsPerAccountHundredths != 20_000 {
		t.Fatalf("credits_per_account_hundredths = %d, want 20_000", created.CreditsPerAccountHundredths)
	}
	// Derived unit cost: 5_610_000 / 200 = 28_050
	if created.UnitCostMicros != 28_050 {
		t.Fatalf("unit_cost_micros = %d, want 28_050", created.UnitCostMicros)
	}
	if !created.Active || created.PricingClass != "normal" {
		t.Fatalf("defaults not applied: active=%v class=%q", created.Active, created.PricingClass)
	}
	if created.ID == "" {
		t.Fatalf("id should be auto-generated")
	}

	// Now GET and verify the summary reflects the batch (not the config fallback).
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/purchase-batches", nil))
	var listed struct {
		Data    []PurchaseBatchView `json:"data"`
		Summary map[string]any      `json:"summary"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(listed.Data))
	}
	if listed.Summary["fallback_applied"] != false {
		t.Fatalf("fallback should NOT apply after creating a batch")
	}
	if listed.Summary["effective_unit_cost_micros"] != float64(28_050) {
		t.Fatalf("effective_unit_cost = %v, want 28050", listed.Summary["effective_unit_cost_micros"])
	}
}

// TestPurchaseBatches_UpdateMerges verifies PUT's partial-update
// semantics: send only `active=false` and confirm the row's other
// fields are preserved.
func TestPurchaseBatches_UpdateMerges(t *testing.T) {
	store := &purchaseBatchesStore{
		rows: []domain.PurchaseBatch{{
			ID:                          "pb_x",
			SourceChannel:               "tg",
			PlanType:                    "starter",
			AccountsCount:               1,
			CreditsPerAccountHundredths: 20_000,
			TotalPaidMicros:             5_610_000,
			PricingClass:                "normal",
			Active:                      true,
			PaidCurrency:                "USD",
			ExchangeRateUsed:            1.0,
			Rationale:                   "original note",
		}},
	}
	r := newPurchaseBatchesHandler(store)

	// Only flip active → false. Rationale MUST survive.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/pricing/purchase-batches/pb_x",
		bytes.NewBufferString(`{"active": false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.rows) != 1 {
		t.Fatalf("row count = %d", len(store.rows))
	}
	if store.rows[0].Active {
		t.Fatalf("active should be false after PUT")
	}
	if store.rows[0].Rationale != "original note" {
		t.Fatalf("rationale should be preserved, got %q", store.rows[0].Rationale)
	}
	if store.rows[0].TotalPaidMicros != 5_610_000 {
		t.Fatalf("total_paid should be preserved")
	}
}

// TestPurchaseBatches_UpdateUnknownIs404 covers the "operator clicks
// edit on a row someone else deleted" race.
func TestPurchaseBatches_UpdateUnknownIs404(t *testing.T) {
	r := newPurchaseBatchesHandler(&purchaseBatchesStore{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/pricing/purchase-batches/nope",
		bytes.NewBufferString(`{"active": false}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

// TestPurchaseBatches_DeleteIdempotent proves DELETE returns 204 for
// both existing and unknown rows. The store contract in
// ports/storage.go says the delete is a no-op when the id doesn't
// exist; the handler must not turn a nil-error into a spurious 500.
func TestPurchaseBatches_DeleteIdempotent(t *testing.T) {
	store := &purchaseBatchesStore{
		rows: []domain.PurchaseBatch{{ID: "pb_x", SourceChannel: "tg", PlanType: "starter",
			AccountsCount: 1, TotalPaidMicros: 1, PricingClass: "normal", Active: true, PaidCurrency: "USD"}},
	}
	r := newPurchaseBatchesHandler(store)

	// Existing row.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/pricing/purchase-batches/pb_x", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("existing delete status=%d", rec.Code)
	}
	if len(store.rows) != 0 {
		t.Fatalf("row should have been removed")
	}

	// Unknown row — still 204.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/pricing/purchase-batches/pb_unknown", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unknown delete status=%d, want 204 (idempotent)", rec.Code)
	}
}

// TestPurchaseBatches_ValidationErrors sweeps the field-level 400s
// the request validator raises. Each sub-test asserts the SPECIFIC
// error message references the offending field so the UI can flag
// it inline.
func TestPurchaseBatches_ValidationErrors(t *testing.T) {
	r := newPurchaseBatchesHandler(&purchaseBatchesStore{})
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"missing_channel", `{"plan_type":"starter","total_paid_usd":1}`, "source_channel"},
		{"missing_plan", `{"source_channel":"tg","total_paid_usd":1}`, "plan_type"},
		{"zero_paid", `{"source_channel":"tg","plan_type":"starter","total_paid_usd":0}`, "total_paid"},
		{"zero_accounts", `{"source_channel":"tg","plan_type":"starter","total_paid_usd":1,"accounts_count":0}`, "accounts_count"},
		{"bad_class", `{"source_channel":"tg","plan_type":"starter","total_paid_usd":1,"pricing_class":"weird"}`, "pricing_class"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pricing/purchase-batches",
				bytes.NewBufferString(tc.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if !bytes.Contains(rec.Body.Bytes(), []byte(tc.wantErr)) {
				t.Fatalf("error should mention %q, got %s", tc.wantErr, rec.Body.String())
			}
		})
	}
}

// TestPurchaseBatches_PromotionType_Unlim1Day exercises the fixed
// unlim_1day model: base plan is still starter (200 credits), but
// promotion_type="unlim_1day" flags the 1-day-unlim promo layer.
// The row MUST NOT contribute to the weighted average even though
// it has non-zero credits — the calculator filters on
// promotion_type=="none" before touching the denominator.
func TestPurchaseBatches_PromotionType_Unlim1Day(t *testing.T) {
	r := newPurchaseBatchesHandler(&purchaseBatchesStore{})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pricing/purchase-batches",
		bytes.NewBufferString(`{
			"source_channel": "tg",
			"source_seller": "CheapLuxuryAI",
			"plan_type": "starter",
			"promotion_type": "unlim_1day",
			"credits_per_account": 200,
			"total_paid_usd": 2.86,
			"linked_account_email": "unlim@example.com"
		}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var view PurchaseBatchView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.PromotionType != "unlim_1day" {
		t.Fatalf("promotion_type = %q, want unlim_1day", view.PromotionType)
	}
	if view.PlanType != "starter" {
		t.Fatalf("plan_type = %q, want starter", view.PlanType)
	}
	// Now GET and confirm the summary shows fallback_applied=true
	// (the promo row is excluded → no eligible batches).
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pricing/purchase-batches", nil))
	var body struct {
		Summary map[string]any `json:"summary"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Summary["fallback_applied"] != true {
		t.Fatalf("fallback should apply — promo row must not count toward weighted average; summary=%+v", body.Summary)
	}
	if body.Summary["eligible_batches"] != float64(0) {
		t.Fatalf("eligible = %v, want 0", body.Summary["eligible_batches"])
	}
}

// TestPurchaseBatches_ValidPromoTypes sweeps the four allowed
// promotion_type values (none/first_signup/unlim_1day/standard_credit_boost).
// Any other value must 400 with a message pointing at promotion_type.
func TestPurchaseBatches_ValidPromoTypes(t *testing.T) {
	r := newPurchaseBatchesHandler(&purchaseBatchesStore{})
	base := `{
		"source_channel": "tg",
		"plan_type": "starter",
		"total_paid_usd": 5.61,
		"credits_per_account": 200,
		"promotion_type": %q
	}`
	for _, valid := range []string{"none", "first_signup", "unlim_1day", "standard_credit_boost"} {
		t.Run("valid_"+valid, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pricing/purchase-batches",
				bytes.NewBufferString(sprintf(base, valid))))
			if rec.Code != http.StatusCreated {
				t.Fatalf("promotion_type=%q status=%d body=%s", valid, rec.Code, rec.Body.String())
			}
		})
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pricing/purchase-batches",
		bytes.NewBufferString(sprintf(base, "bogus"))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus promotion_type should 400, got %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("promotion_type")) {
		t.Fatalf("error should mention promotion_type, got %s", rec.Body.String())
	}
}

// sprintf is a tiny local helper so the test doesn't need fmt import
// clutter beyond what's already implied by the other test-fixture
// bodies.
func sprintf(tmpl string, args ...any) string {
	return fmt.Sprintf(tmpl, args...)
}
