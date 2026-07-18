package admin

// Tests for the routing_strategy_default fallback in POST /admin/groups.
// The handler must:
//   1. Prefer an explicit route_strategy on the request body.
//   2. When absent, resolve the default via the SettingsStore.
//   3. When no store is wired, fall back to domain.RouteRoundRobin
//      (matching the prior hard-coded behaviour).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// newGroupsRouterWithSettings builds a router whose GroupsHandler has
// the Settings field populated. Kept separate from newGroupsRouter to
// keep the existing tests unchanged.
func newGroupsRouterWithSettings(t *testing.T, store *memSettingsStore) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	gh := NewGroupsHandler(newFakeGroupStore())
	gh.Settings = store
	gh.Register(r)
	return r
}

func TestGroupsHandler_Create_UsesSettingsDefault_WhenNoRouteStrategy(t *testing.T) {
	store := newMemSettingsStore()
	if err := store.Set(context.Background(), SettingKeyRoutingStrategyDefault, "priority"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	r := newGroupsRouterWithSettings(t, store)

	req := httptest.NewRequest(http.MethodPost, "/groups",
		bytes.NewBufferString(`{"name":"payments-team"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view["route_strategy"] != string(domain.RoutePriority) {
		t.Errorf("route_strategy: got %v want %s (mapped from settings default)",
			view["route_strategy"], domain.RoutePriority)
	}
}

func TestGroupsHandler_Create_ExplicitRouteStrategyOverridesSettingsDefault(t *testing.T) {
	store := newMemSettingsStore()
	if err := store.Set(context.Background(), SettingKeyRoutingStrategyDefault, "priority"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	r := newGroupsRouterWithSettings(t, store)

	// Explicit least_used on the body should win.
	req := httptest.NewRequest(http.MethodPost, "/groups",
		bytes.NewBufferString(`{"name":"team-alpha","route_strategy":"least_used"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view["route_strategy"] != "least_used" {
		t.Errorf("route_strategy: got %v want least_used (explicit override)", view["route_strategy"])
	}
}

func TestGroupsHandler_Create_FallsBackToRoundRobin_WhenSettingsMissing(t *testing.T) {
	// Settings store is wired but has no row for the routing key.
	store := newMemSettingsStore()
	r := newGroupsRouterWithSettings(t, store)

	req := httptest.NewRequest(http.MethodPost, "/groups",
		bytes.NewBufferString(`{"name":"greenfield"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view["route_strategy"] != string(domain.RouteRoundRobin) {
		t.Errorf("route_strategy: got %v want %s (fallback)",
			view["route_strategy"], domain.RouteRoundRobin)
	}
}

func TestGroupsHandler_Create_FallsBackToRoundRobin_WhenSettingsNil(t *testing.T) {
	// No Settings field wired at all (slimmer deployment).
	r := chi.NewRouter()
	NewGroupsHandler(newFakeGroupStore()).Register(r)

	req := httptest.NewRequest(http.MethodPost, "/groups",
		bytes.NewBufferString(`{"name":"lean-deploy"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view["route_strategy"] != string(domain.RouteRoundRobin) {
		t.Errorf("route_strategy: got %v want %s (nil store fallback)",
			view["route_strategy"], domain.RouteRoundRobin)
	}
}

// TestGroupsHandler_Create_IgnoresCorruptedSettingsRow ensures a
// legacy hand-edited system_settings row (e.g. a raw route_strategy
// value like "least_used") does not leak into a new group's
// route_strategy. The Create path should treat the corrupted row as
// "no default" and fall back to round_robin.
func TestGroupsHandler_Create_IgnoresCorruptedSettingsRow(t *testing.T) {
	store := newMemSettingsStore()
	// Legacy operator wrote a group-level enum into the settings row.
	if err := store.Set(context.Background(), SettingKeyRoutingStrategyDefault, "least_used"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := newGroupsRouterWithSettings(t, store)

	req := httptest.NewRequest(http.MethodPost, "/groups",
		bytes.NewBufferString(`{"name":"post-migration"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d want 201; body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view["route_strategy"] != string(domain.RouteRoundRobin) {
		t.Errorf("route_strategy: got %v want %s (corrupted row fallback)",
			view["route_strategy"], domain.RouteRoundRobin)
	}
}
