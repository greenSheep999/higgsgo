package api

// Regression test for the /v1/videos vs /v1/video split.
//
// higgsgo shipped its OpenAI-style public surface with plural
// `/v1/videos/generations` from day one — see docs/CONVENTIONS.md's
// "OpenAI-compatible naming" note. new-api / OneAPI adopters, however,
// speak the singular `/v1/video/generations`. Rather than break the
// legacy callers by renaming, server.publicRouter() now mounts *both*
// paths against the same HandleVideoGeneration.
//
// This test guards that alias in the cheapest way possible: send an
// empty-body POST to each path and assert we did NOT hit chi's
// NotFoundHandler (404). What we DO get depends on wiring: with
// APIKeys=nil the auth middleware short-circuits to next and the
// handler answers 400 `invalid_body`; with a real APIKeys store the
// same request would 401 at auth. Both outcomes are equally good
// proof that "the route exists and the middleware chain engaged" —
// the invariant callers care about. If someone accidentally deletes
// one of the r.Post lines during a future refactor, this test flips
// to 404 and the regression is loud.

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/greensheep999/higgsgo/internal/api/v1"
	"github.com/greensheep999/higgsgo/internal/config"
)

// newVideoAliasTestServer builds the minimum Server needed to wire the
// /v1 routes. Handler methods themselves are never invoked (auth
// middleware short-circuits first), so a zero-value Handler is fine —
// we only need the method values as router registrations.
func newVideoAliasTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.AdminListen = "127.0.0.1:0"
	// RateLimit stays at zero-value: unused because auth 401s first.
	return &Server{
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		V1:     &v1.Handler{},
		// APIKeys intentionally nil — the middleware path used in
		// publicRouter() short-circuits with a 401 when Authorization
		// is missing, before it would consult the store.
	}
}

// TestPublicRouter_VideoAliasBothPathsMounted asserts that BOTH
// `/v1/videos/generations` (legacy) and `/v1/video/generations`
// (new-api compatible) resolve to a routed handler. Getting a 401
// (not 404) proves the mux matched.
func TestPublicRouter_VideoAliasBothPathsMounted(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"legacy_plural", "/v1/videos/generations"},
		{"new_api_singular", "/v1/video/generations"},
	}
	s := newVideoAliasTestServer(t)
	router := s.publicRouter()

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			// No Authorization header on purpose.
			router.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound {
				t.Fatalf("path %s returned 404 — route is not mounted; got body=%q",
					tc.path, rec.Body.String())
			}
			// A 400 (invalid_body from the handler because our POST
			// carried no JSON) or 401 (auth rejection) are both fine
			// — both prove the route existed and the middleware chain
			// ran. The one thing we refuse is 404.
		})
	}
}

// TestPublicRouter_UnknownVideoPathIs404 is the negative half of the
// mux-shape check: a path that intentionally isn't wired must still
// 404. Guards against a future "catch-all" refactor that would make
// the alias test above pass trivially.
func TestPublicRouter_UnknownVideoPathIs404(t *testing.T) {
	s := newVideoAliasTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/vid/generations", nil)
	s.publicRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown video path returned %d, want 404 (body=%q)",
			rec.Code, rec.Body.String())
	}
}
