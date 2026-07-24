package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClient_FetchFreeGensV2_Success asserts the happy path: the endpoint's
// {items:[{job_set_type,counter,config}]} envelope collapses to a
// job_set_type → counter map. Fixture mirrors the starter-tier snapshot in
// server/data/user-entitlements.json.
func TestClient_FetchFreeGensV2_Success(t *testing.T) {
	fixture := `{"items":[
		{"job_set_type":"text2image_soul_v2","counter":298,"config":null},
		{"job_set_type":"soul_cinematic","counter":298,"config":null},
		{"job_set_type":"soul_location","counter":298,"config":null}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/free-gens/v2" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.FetchFreeGensV2(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchFreeGensV2: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(got), got)
	}
	for _, jst := range []string{"text2image_soul_v2", "soul_cinematic", "soul_location"} {
		if got[jst] != 298 {
			t.Errorf("%s: got %v want 298", jst, got[jst])
		}
	}
}

// TestClient_FetchFreeGensV2_Empty asserts an empty items array yields a
// benign nil map (no error), matching the FetchGifts nil-slice convention.
func TestClient_FetchFreeGensV2_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.FetchFreeGensV2(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchFreeGensV2: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil map for empty items, got %v", got)
	}
}

// TestClient_FetchFreeGensV2_NotFound asserts a 404 (surface absent for the
// account) is treated as a benign nil map rather than an error.
func TestClient_FetchFreeGensV2_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.FetchFreeGensV2(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchFreeGensV2 on 404 should be benign: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil map on 404, got %v", got)
	}
}
