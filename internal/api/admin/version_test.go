package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/version"
)

// newVersionRouter builds a chi router with a VersionHandler pointed at the
// given mock github URL. Cache TTL is short so cache-expiry cases can be
// exercised without racing a wall clock.
func newVersionRouter(t *testing.T, mockGitHub string, checkEnabled bool) (*VersionHandler, chi.Router) {
	t.Helper()
	h := &VersionHandler{
		GitHubOwner:   "greenSheep999",
		GitHubRepo:    "higgsgo",
		CheckEnabled:  checkEnabled,
		HTTPClient:    &http.Client{Timeout: 2 * time.Second},
		GitHubBaseURL: mockGitHub,
		CacheTTL:      50 * time.Millisecond,
	}
	r := chi.NewRouter()
	h.Register(r)
	return h, r
}

// withVersion swaps the package-level Version for the duration of a test.
// Restores on Cleanup. Not parallel-safe — the package variable is global.
func withVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = orig })
}

func TestVersion_Current(t *testing.T) {
	withVersion(t, "v0.2.0")

	_, r := newVersionRouter(t, "", true)
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["version"] != "v0.2.0" {
		t.Errorf("version: got %v want v0.2.0", body["version"])
	}
	if body["go_version"] != runtime.Version() {
		t.Errorf("go_version: got %v want %s", body["go_version"], runtime.Version())
	}
	if body["os_arch"] != runtime.GOOS+"/"+runtime.GOARCH {
		t.Errorf("os_arch: got %v want %s/%s", body["os_arch"], runtime.GOOS, runtime.GOARCH)
	}
	if _, ok := body["commit"]; !ok {
		t.Errorf("commit missing")
	}
	if _, ok := body["build_time"]; !ok {
		t.Errorf("build_time missing")
	}
}

func TestVersionCheck_DevShortCircuit(t *testing.T) {
	withVersion(t, "dev")

	// Even with an obviously bogus mock GitHub URL (we're going to
	// assert the handler NEVER hits it), the request should return
	// a dev-mode response with update_available=false.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)

	_, r := newVersionRouter(t, srv.URL, true)
	req := httptest.NewRequest(http.MethodGet, "/version/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Errorf("dev short-circuit failed: handler hit mock GitHub")
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["dev"] != true {
		t.Errorf("dev: got %v want true", body["dev"])
	}
	if body["update_available"] != false {
		t.Errorf("update_available: got %v want false", body["update_available"])
	}
	if body["current"] != "dev" {
		t.Errorf("current: got %v want dev", body["current"])
	}
}

func TestVersionCheck_Disabled(t *testing.T) {
	withVersion(t, "v0.2.0")

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)

	_, r := newVersionRouter(t, srv.URL, false /* CheckEnabled */)
	req := httptest.NewRequest(http.MethodGet, "/version/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if called {
		t.Errorf("check=false but handler hit GitHub")
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["update_available"] != false {
		t.Errorf("update_available: got %v want false", body["update_available"])
	}
	if body["current"] != "v0.2.0" {
		t.Errorf("current: got %v want v0.2.0", body["current"])
	}
}

func TestVersionCheck_UpdateAvailable(t *testing.T) {
	withVersion(t, "v0.2.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity-check the request the handler makes.
		wantPath := "/repos/greenSheep999/higgsgo/releases/latest"
		if r.URL.Path != wantPath {
			t.Errorf("path: got %q want %q", r.URL.Path, wantPath)
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "higgsgo/") {
			t.Errorf("User-Agent: got %q want higgsgo/<version>", ua)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v0.3.1",
			"html_url":     "https://github.com/greenSheep999/higgsgo/releases/tag/v0.3.1",
			"published_at": "2026-07-18T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)

	_, r := newVersionRouter(t, srv.URL, true)
	req := httptest.NewRequest(http.MethodGet, "/version/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["current"] != "v0.2.0" {
		t.Errorf("current: got %v want v0.2.0", body["current"])
	}
	if body["latest"] != "v0.3.1" {
		t.Errorf("latest: got %v want v0.3.1", body["latest"])
	}
	if body["update_available"] != true {
		t.Errorf("update_available: got %v want true", body["update_available"])
	}
	if body["release_url"] != "https://github.com/greenSheep999/higgsgo/releases/tag/v0.3.1" {
		t.Errorf("release_url: got %v", body["release_url"])
	}
	if body["published_at"] != "2026-07-18T00:00:00Z" {
		t.Errorf("published_at: got %v", body["published_at"])
	}
	if _, hasErr := body["error"]; hasErr {
		t.Errorf("error should be absent on success")
	}
}

func TestVersionCheck_NoUpdate(t *testing.T) {
	withVersion(t, "v0.5.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v0.5.0",
			"html_url":     "https://github.com/greenSheep999/higgsgo/releases/tag/v0.5.0",
			"published_at": "2026-06-01T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)

	_, r := newVersionRouter(t, srv.URL, true)
	req := httptest.NewRequest(http.MethodGet, "/version/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["update_available"] != false {
		t.Errorf("update_available: got %v want false", body["update_available"])
	}
}

func TestVersionCheck_Upstream5xxDegrades(t *testing.T) {
	withVersion(t, "v0.2.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	_, r := newVersionRouter(t, srv.URL, true)
	req := httptest.NewRequest(http.MethodGet, "/version/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 on upstream 5xx", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "upstream_unavailable" {
		t.Errorf("error: got %v want upstream_unavailable", body["error"])
	}
	if body["current"] != "v0.2.0" {
		t.Errorf("current: got %v want v0.2.0", body["current"])
	}
	// Should not claim an update on an empty upstream response.
	if body["update_available"] != false {
		t.Errorf("update_available: got %v want false", body["update_available"])
	}
}

func TestVersionCheck_MalformedBodyDegrades(t *testing.T) {
	withVersion(t, "v0.2.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)

	_, r := newVersionRouter(t, srv.URL, true)
	req := httptest.NewRequest(http.MethodGet, "/version/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 on malformed upstream body", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "upstream_unavailable" {
		t.Errorf("error: got %v want upstream_unavailable", body["error"])
	}
}

func TestVersionCheck_CachesUpstream(t *testing.T) {
	withVersion(t, "v0.2.0")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v0.3.1",
			"html_url":     "https://github.com/greenSheep999/higgsgo/releases/tag/v0.3.1",
			"published_at": "2026-07-18T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)

	h, r := newVersionRouter(t, srv.URL, true)
	// Make the TTL long enough that the second request definitely
	// falls inside the cache window.
	h.CacheTTL = 1 * time.Minute

	// First hit: fetches from mock.
	req := httptest.NewRequest(http.MethodGet, "/version/check", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first hit status: got %d", rec.Code)
	}

	// Second hit: MUST come from cache.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version/check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second hit status: got %d", rec.Code)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits: got %d want 1 (second request should be cached)", got)
	}
}

func TestVersionCheck_ErrorNotCached(t *testing.T) {
	withVersion(t, "v0.2.0")

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	h, r := newVersionRouter(t, srv.URL, true)
	h.CacheTTL = 1 * time.Minute

	// First request: 5xx.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version/check", nil))
	// Second request: should retry upstream (not cache the failure).
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version/check", nil))

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits: got %d want 2 (errors must not be cached)", got)
	}
}
