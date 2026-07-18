package admin

// Tests for RoutingSettingsHandler and the routing_strategy_default
// fallback in POST /admin/groups.
//
// The store contract is exercised via a package-local in-memory
// implementation (memSettingsStore below) so these tests stay
// independent of the sqlite adapter.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// memSettingsStore + newMemSettingsStore live in settings_test.go —
// reused here to avoid a second in-memory ports.SettingsStore fake.

// mountRoutingRouter wraps the handler in a chi router so the routes
// match the /admin prefix production uses.
func mountRoutingRouter(h *RoutingSettingsHandler) http.Handler {
	r := chi.NewRouter()
	r.Route("/admin", func(r chi.Router) { h.Register(r) })
	return r
}

func TestRoutingSettings_GetReturnsDefaultWhenRowMissing(t *testing.T) {
	store := newMemSettingsStore()
	h := NewRoutingSettingsHandler(store)

	req := httptest.NewRequest(http.MethodGet, "/admin/settings/routing", nil)
	rec := httptest.NewRecorder()
	mountRoutingRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["strategy"] != string(DefaultRoutingPreference) {
		t.Errorf("strategy: got %v want %s", body["strategy"], DefaultRoutingPreference)
	}
	if body["source"] != "default" {
		t.Errorf("source: got %v want default", body["source"])
	}
}

func TestRoutingSettings_PutThenGetReflectsDBSource(t *testing.T) {
	store := newMemSettingsStore()
	h := NewRoutingSettingsHandler(store)
	router := mountRoutingRouter(h)

	// PUT priority.
	putReq := httptest.NewRequest(http.MethodPut, "/admin/settings/routing",
		bytes.NewBufferString(`{"strategy":"priority"}`))
	putRec := httptest.NewRecorder()
	router.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status: got %d want 200; body=%s", putRec.Code, putRec.Body.String())
	}
	var putBody map[string]any
	_ = json.Unmarshal(putRec.Body.Bytes(), &putBody)
	if putBody["strategy"] != "priority" {
		t.Errorf("PUT strategy: got %v want priority", putBody["strategy"])
	}
	if putBody["source"] != "db" {
		t.Errorf("PUT source: got %v want db", putBody["source"])
	}

	// GET must now report db as the source and priority as the value.
	getReq := httptest.NewRequest(http.MethodGet, "/admin/settings/routing", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status: got %d want 200; body=%s", getRec.Code, getRec.Body.String())
	}
	var getBody map[string]any
	_ = json.Unmarshal(getRec.Body.Bytes(), &getBody)
	if getBody["strategy"] != "priority" {
		t.Errorf("GET strategy: got %v want priority", getBody["strategy"])
	}
	if getBody["source"] != "db" {
		t.Errorf("GET source: got %v want db", getBody["source"])
	}

	// Underlying store has the raw preference value, not the mapped
	// route_strategy — mapping happens only when creating groups.
	if v, err := store.Get(context.Background(), SettingKeyRoutingStrategyDefault); err != nil || v != "priority" {
		t.Errorf("store: got %q err %v want persisted priority", v, err)
	}
}

func TestRoutingSettings_PutRejectsInvalidStrategy(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty_object", `{}`},
		{"round_robin_leaked_from_domain", `{"strategy":"round_robin"}`},
		{"unknown_value", `{"strategy":"balanced"}`},
		{"empty_string", `{"strategy":""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newMemSettingsStore()
			h := NewRoutingSettingsHandler(store)
			req := httptest.NewRequest(http.MethodPut, "/admin/settings/routing",
				bytes.NewBufferString(c.body))
			rec := httptest.NewRecorder()
			mountRoutingRouter(h).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if got := errType(body); got != "invalid_strategy" {
				t.Errorf("error.type: got %q want invalid_strategy", got)
			}
			// Store must NOT have been touched.
			if _, err := store.Get(context.Background(), SettingKeyRoutingStrategyDefault); !errors.Is(err, domain.ErrSettingNotFound) {
				t.Errorf("store touched after invalid PUT: err=%v", err)
			}
		})
	}
}

func TestRoutingSettings_PutRejectsMalformedJSON(t *testing.T) {
	store := newMemSettingsStore()
	h := NewRoutingSettingsHandler(store)
	req := httptest.NewRequest(http.MethodPut, "/admin/settings/routing",
		bytes.NewBufferString(`{"strategy":`))
	rec := httptest.NewRecorder()
	mountRoutingRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if got := errType(body); got != "invalid_body" {
		t.Errorf("error.type: got %q want invalid_body", got)
	}
}

// TestRoutingSettings_GetTreatsCorruptedRowAsDefault covers the guard
// against a hand-edited system_settings row landing an unrecognised
// value in the DB. The GET path must not surface "priority" or the
// raw value — it should fall back to the sidebar default so the SPA
// keeps rendering a valid radio.
func TestRoutingSettings_GetTreatsCorruptedRowAsDefault(t *testing.T) {
	store := newMemSettingsStore()
	// Simulate a corrupted row (e.g. a legacy operator hand-editing
	// system_settings with the group-level enum).
	_ = store.Set(context.Background(), SettingKeyRoutingStrategyDefault, "round_robin")

	h := NewRoutingSettingsHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/admin/settings/routing", nil)
	rec := httptest.NewRecorder()
	mountRoutingRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["strategy"] != string(DefaultRoutingPreference) {
		t.Errorf("strategy: got %v want %s (fallback)", body["strategy"], DefaultRoutingPreference)
	}
	if body["source"] != "default" {
		t.Errorf("source: got %v want default (fallback)", body["source"])
	}
}

func TestPreferenceToRouteStrategyMapping(t *testing.T) {
	cases := []struct {
		in   RoutingPreference
		want domain.RouteStrategy
	}{
		{RoutingPreferenceLoadBalance, domain.RouteRoundRobin},
		{RoutingPreferencePriority, domain.RoutePriority},
		{RoutingPreference(""), domain.RouteRoundRobin},
		{RoutingPreference("bogus"), domain.RouteRoundRobin},
	}
	for _, c := range cases {
		if got := PreferenceToRouteStrategy(c.in); got != c.want {
			t.Errorf("PreferenceToRouteStrategy(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// TestResolveDefaultRouteStrategy_NilStore locks in the "nil store →
// round_robin" contract so a slimmer deployment that skips persistence
// still lands sane group defaults.
func TestResolveDefaultRouteStrategy_NilStore(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := ResolveDefaultRouteStrategy(req, nil); got != domain.RouteRoundRobin {
		t.Errorf("nil store: got %q want round_robin", got)
	}
}

// TestResolveDefaultRouteStrategy_ReadsPersistedPreference covers the
// full mapping through the SettingsStore.
func TestResolveDefaultRouteStrategy_ReadsPersistedPreference(t *testing.T) {
	store := newMemSettingsStore()
	_ = store.Set(context.Background(), SettingKeyRoutingStrategyDefault, "priority")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := ResolveDefaultRouteStrategy(req, store); got != domain.RoutePriority {
		t.Errorf("stored priority: got %q want priority", got)
	}
}

// Compile-time guarantee: memSettingsStore satisfies the
// ports.SettingsStore interface. Prevents a signature drift from
// silently disabling half the tests.
var _ = func() bool {
	var _ interface {
		Get(context.Context, string) (string, error)
		Set(context.Context, string, string) error
		UpdatedAt(context.Context, string) (time.Time, error)
	} = (*memSettingsStore)(nil)
	return true
}()

// errType lives in settings_test.go — reused here.
