package upstream

// Tests for FetchJobSetCosts: the GET /job-sets/costs fetch + the
// reduceJobSetCost union-shape collapse to credit-hundredths.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// costsFixture covers all four documented `cost` item variants so the
// reducer is exercised against every shape it must survive.
const costsFixture = `{
  "data": [
    {"job_set_type": "kling3_0", "cost": [
      {"mode": "pro", "audio": {"on": 2.5, "off": 1.75}},
      {"mode": "std", "audio": {"on": 2, "off": 1.5}}
    ]},
    {"job_set_type": "seedance_2_0", "cost": [
      {"model": "seedance_2_0", "resolutions": [
        {"resolution": "480p", "cost_per_second": 3, "original_cost_per_second": 6},
        {"resolution": "1080p", "cost_per_second": 9, "original_cost_per_second": 12}
      ]},
      {"model": "seedance_2_0_fast", "resolutions": [
        {"resolution": "480p", "cost_per_second": 1.5, "original_cost_per_second": 1.5}
      ]}
    ]},
    {"job_set_type": "cinematic_studio_3_0", "cost": [
      {"resolution": "480p", "cost_per_second": 3.5, "original_cost_per_second": 6},
      {"resolution": "4k", "cost_per_second": 24, "original_cost_per_second": 26}
    ]},
    {"job_set_type": "recraft_v4_1", "cost": [
      {"model_type": "standard", "resolution": "1k", "credits": 1.25},
      {"model_type": "vector", "resolution": "2k", "credits": 10}
    ]},
    {"job_set_type": "empty_model", "cost": []}
  ]
}`

func TestClient_FetchJobSetCosts_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/job-sets/costs" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(costsFixture))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	costs, err := c.FetchJobSetCosts(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchJobSetCosts: %v", err)
	}

	// Reducer takes the MIN strictly-positive cost across every variant
	// field, ×100 → hundredths.
	want := map[string]int64{
		"kling3_0":             150, // min(2.5,1.75,2,1.5) = 1.5
		"seedance_2_0":         150, // min(3,9,1.5) = 1.5
		"cinematic_studio_3_0": 350, // min(3.5,24) = 3.5
		"recraft_v4_1":         125, // min(1.25,10) = 1.25
	}
	if len(costs) != len(want) {
		t.Fatalf("got %d entries %v, want %d", len(costs), costs, len(want))
	}
	for jst, exp := range want {
		if got := costs[jst]; got != exp {
			t.Errorf("cost[%s] = %d, want %d", jst, got, exp)
		}
	}
	// empty_model has no positive cost field → dropped from the map.
	if _, ok := costs["empty_model"]; ok {
		t.Errorf("empty_model should be dropped (no positive cost), got entry")
	}
}

func TestClient_FetchJobSetCostCatalog_NormalizesDimensions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(costsFixture))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	catalog, err := c.FetchJobSetCostCatalog(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchJobSetCostCatalog: %v", err)
	}
	if catalog == nil {
		t.Fatal("catalog is nil")
	}
	if catalog.RawJSON == "" {
		t.Fatal("raw upstream payload was not retained")
	}
	if got, want := len(catalog.Rules), 11; got != want {
		t.Fatalf("rule count = %d, want %d; rules=%+v", got, want, catalog.Rules)
	}

	assertRule := func(jst, unit, component, resolution, mode, audio string, credits, original int64) {
		t.Helper()
		for _, rule := range catalog.Rules {
			if rule.JST == jst && rule.Unit == unit && rule.Component == component &&
				rule.Resolution == resolution && rule.Mode == mode && rule.Audio == audio &&
				rule.CreditsHundredths == credits && rule.OriginalCreditsHundredths == original {
				return
			}
		}
		t.Errorf("missing rule jst=%s unit=%s component=%s resolution=%s mode=%s audio=%s credits=%d original=%d; rules=%+v",
			jst, unit, component, resolution, mode, audio, credits, original, catalog.Rules)
	}

	assertRule("seedance_2_0", "per_second", "cost_per_second", "1080p", "", "", 900, 1200)
	assertRule("cinematic_studio_3_0", "per_second", "cost_per_second", "4k", "", "", 2400, 2600)
	assertRule("kling3_0", "upstream_unspecified", "audio_state", "", "pro", "on", 250, 0)
	assertRule("kling3_0", "upstream_unspecified", "audio_state", "", "std", "off", 150, 0)
	assertRule("recraft_v4_1", "per_request", "credits", "2k", "", "", 1000, 0)
}

func TestClient_FetchJobSetCosts_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data": []}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	costs, err := c.FetchJobSetCosts(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchJobSetCosts: %v", err)
	}
	if costs != nil {
		t.Errorf("empty data should return nil map, got %v", costs)
	}
}

func TestClient_FetchJobSetCosts_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.FetchJobSetCosts(context.Background(), testAccount()); err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}
