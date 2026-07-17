package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// newStatsRouter mounts a StatsHandler. Jobs is left nil because StatsHandler
// does not use it in the endpoints under test — matches the "optional Jobs"
// contract on the struct.
func newStatsRouter(store ports.AccountStore) chi.Router {
	r := chi.NewRouter()
	h := NewStatsHandler(store, nil)
	h.Register(r)
	return r
}

func TestStatsHandler_Pool_Aggregates(t *testing.T) {
	// Fixture:
	//   plans        : 2x plus, 1x pro, 1x starter
	//   statuses     : 3x active, 1x suspended
	//   sub balances : 10000 + 5000 + 8000 + 2000 = 25000 (credits*100)
	//                  -> display value 250.00
	//   unlim flags  : 1 has_unlim, 1 has_flex_unlim
	rows := []domain.Account{
		{ID: "user_1", PlanType: domain.PlanPlus, Status: domain.StatusActive, SubscriptionBalance: 10000, HasUnlim: true},
		{ID: "user_2", PlanType: domain.PlanPlus, Status: domain.StatusActive, SubscriptionBalance: 5000},
		{ID: "user_3", PlanType: domain.PlanPro, Status: domain.StatusActive, SubscriptionBalance: 8000, HasFlexUnlim: true},
		{ID: "user_4", PlanType: domain.PlanStarter, Status: domain.StatusSuspended, SubscriptionBalance: 2000},
	}
	store := &fakeAccountStore{listRows: rows}
	r := newStatsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/stats/pool", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got, want := body["total"], float64(4); got != want {
		t.Errorf("total: got %v want %v", got, want)
	}

	byPlan, ok := body["by_plan"].(map[string]any)
	if !ok {
		t.Fatalf("by_plan missing or wrong type: %T", body["by_plan"])
	}
	wantByPlan := map[string]float64{"plus": 2, "pro": 1, "starter": 1}
	for k, want := range wantByPlan {
		if got := byPlan[k]; got != want {
			t.Errorf("by_plan[%q]: got %v want %v", k, got, want)
		}
	}

	byStatus, ok := body["by_status"].(map[string]any)
	if !ok {
		t.Fatalf("by_status missing or wrong type: %T", body["by_status"])
	}
	wantByStatus := map[string]float64{"active": 3, "suspended": 1}
	for k, want := range wantByStatus {
		if got := byStatus[k]; got != want {
			t.Errorf("by_status[%q]: got %v want %v", k, got, want)
		}
	}

	// 25000 hundredths -> 250.00 display credits.
	if got, want := body["total_subscription_balance"], 250.0; got != want {
		t.Errorf("total_subscription_balance: got %v want %v", got, want)
	}
	if got, want := body["with_unlim"], float64(1); got != want {
		t.Errorf("with_unlim: got %v want %v", got, want)
	}
	if got, want := body["with_flex_unlim"], float64(1); got != want {
		t.Errorf("with_flex_unlim: got %v want %v", got, want)
	}
}

func TestStatsHandler_Health(t *testing.T) {
	// Health calls List with Status=active; the fake returns whatever rows
	// we hand it regardless of filter, which is fine because the handler
	// counts len(rows) rather than filtering itself.
	rows := []domain.Account{
		{ID: "user_1", Status: domain.StatusActive},
		{ID: "user_2", Status: domain.StatusActive},
		{ID: "user_3", Status: domain.StatusActive},
	}
	store := &fakeAccountStore{listRows: rows}
	r := newStatsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/stats/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Handler should ask the store for active-only rows.
	if got, want := store.lastFilter.Status, domain.StatusActive; got != want {
		t.Errorf("filter.Status: got %q want %q", got, want)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok: got %v want true", body["ok"])
	}
	if got, want := body["accounts_active"], float64(3); got != want {
		t.Errorf("accounts_active: got %v want %v", got, want)
	}
	ts, ok := body["time"].(string)
	if !ok {
		t.Fatalf("time missing or wrong type: %T", body["time"])
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("time not RFC3339: %q (%v)", ts, err)
	}
}

func TestStatsHandler_Pool_StoreError(t *testing.T) {
	store := &fakeAccountStore{listErr: errors.New("boom")}
	r := newStatsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/stats/pool", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body.error missing or wrong type: %T", body["error"])
	}
	if errObj["type"] != "internal" {
		t.Errorf("error.type: got %v want internal", errObj["type"])
	}
}
