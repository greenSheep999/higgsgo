package admin

// Tests for LoadBalanceSettingsHandler: default-on-empty, PUT + GET
// round-trip, and headroom validation. The in-memory store fake is
// reused from settings_test.go (memSettingsStore).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/loadbalance"
)

func mountLoadBalanceRouter(h *LoadBalanceSettingsHandler) http.Handler {
	r := chi.NewRouter()
	r.Route("/admin", func(r chi.Router) { h.Register(r) })
	return r
}

func TestLoadBalanceSettings_GetReturnsDefaultsWhenEmpty(t *testing.T) {
	store := newMemSettingsStore()
	h := NewLoadBalanceSettingsHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/admin/settings/load_balance", nil)
	rec := httptest.NewRecorder()
	mountLoadBalanceRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["source"] != "default" {
		t.Errorf("source: got %v want default", body["source"])
	}
	if body["tier_aware"] != true {
		t.Errorf("tier_aware default: got %v want true", body["tier_aware"])
	}
	if body["jitter"] != true {
		t.Errorf("jitter default: got %v want true", body["jitter"])
	}
	if body["prefer_richer"] != false {
		t.Errorf("prefer_richer default: got %v want false", body["prefer_richer"])
	}
	// balance_headroom_pct arrives as float64 through the untyped map.
	if v, ok := body["balance_headroom_pct"].(float64); !ok || int(v) != loadbalance.DefaultBalanceHeadroomPct {
		t.Errorf("balance_headroom_pct default: got %v want %d", body["balance_headroom_pct"], loadbalance.DefaultBalanceHeadroomPct)
	}
}

func TestLoadBalanceSettings_PutThenGetReflectsDBSource(t *testing.T) {
	store := newMemSettingsStore()
	h := NewLoadBalanceSettingsHandler(store)
	router := mountLoadBalanceRouter(h)

	putBody := `{
		"tier_aware": false,
		"prefer_unlim": true,
		"prefer_free_quota": false,
		"prefer_richer": true,
		"balance_headroom_pct": 150,
		"jitter": false
	}`
	putReq := httptest.NewRequest(http.MethodPut, "/admin/settings/load_balance",
		bytes.NewBufferString(putBody))
	putRec := httptest.NewRecorder()
	router.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status: got %d want 200; body=%s", putRec.Code, putRec.Body.String())
	}
	var put map[string]any
	_ = json.Unmarshal(putRec.Body.Bytes(), &put)
	if put["source"] != "db" {
		t.Errorf("PUT source: got %v want db", put["source"])
	}
	if put["tier_aware"] != false {
		t.Errorf("PUT tier_aware: got %v want false", put["tier_aware"])
	}
	if v, _ := put["balance_headroom_pct"].(float64); int(v) != 150 {
		t.Errorf("PUT balance_headroom_pct: got %v want 150", put["balance_headroom_pct"])
	}

	// GET should now reflect the DB state.
	getReq := httptest.NewRequest(http.MethodGet, "/admin/settings/load_balance", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status: got %d want 200; body=%s", getRec.Code, getRec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(getRec.Body.Bytes(), &got)
	if got["source"] != "db" {
		t.Errorf("GET source: got %v want db", got["source"])
	}
	if got["tier_aware"] != false {
		t.Errorf("GET tier_aware: got %v want false", got["tier_aware"])
	}
	if got["prefer_unlim"] != true {
		t.Errorf("GET prefer_unlim: got %v want true", got["prefer_unlim"])
	}
	if got["prefer_richer"] != true {
		t.Errorf("GET prefer_richer: got %v want true", got["prefer_richer"])
	}
	if got["jitter"] != false {
		t.Errorf("GET jitter: got %v want false", got["jitter"])
	}
	if v, _ := got["balance_headroom_pct"].(float64); int(v) != 150 {
		t.Errorf("GET balance_headroom_pct: got %v want 150", got["balance_headroom_pct"])
	}
}

func TestLoadBalanceSettings_PutRejectsHeadroomOutOfRange(t *testing.T) {
	store := newMemSettingsStore()
	h := NewLoadBalanceSettingsHandler(store)
	router := mountLoadBalanceRouter(h)

	cases := []struct {
		name string
		pct  int
	}{
		{"below_min", 99},
		{"above_max", 501},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"tier_aware":           true,
				"prefer_unlim":         false,
				"prefer_free_quota":    false,
				"prefer_richer":        false,
				"balance_headroom_pct": tc.pct,
				"jitter":               true,
			}
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest(http.MethodPut, "/admin/settings/load_balance",
				bytes.NewReader(raw))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
			}
			var envelope map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
			errObj, _ := envelope["error"].(map[string]any)
			if errObj["type"] != "invalid_headroom" {
				t.Errorf("error.type: got %v want invalid_headroom", errObj["type"])
			}
		})
	}
}

func TestLoadBalanceSettings_PutRejectsEmptyBody(t *testing.T) {
	store := newMemSettingsStore()
	h := NewLoadBalanceSettingsHandler(store)
	router := mountLoadBalanceRouter(h)

	req := httptest.NewRequest(http.MethodPut, "/admin/settings/load_balance",
		bytes.NewBufferString(""))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}
