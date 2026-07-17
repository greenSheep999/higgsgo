package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// nextOK is a tiny stand-in for the wrapped handler. Its body doubles as
// proof that non-preflight requests reach the downstream handler and that
// preflight requests do not.
func nextOK() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestCORS_AllowsListedOrigin(t *testing.T) {
	c := &CORS{AllowedOrigins: []string{"http://localhost:5173"}}
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()

	c.Middleware(nextOK()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("ACAO: want %q, got %q", "http://localhost:5173", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC: want %q, got %q", "true", got)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body: want next handler to run, got %q", rec.Body.String())
	}
}

func TestCORS_RejectsUnlistedOrigin(t *testing.T) {
	c := &CORS{AllowedOrigins: []string{"http://foo.com"}}
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "http://bar.com")
	rec := httptest.NewRecorder()

	c.Middleware(nextOK()).ServeHTTP(rec, req)

	// Pass-through: next still runs, but no CORS headers are added.
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO: want empty, got %q", got)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body: want next handler to run, got %q", rec.Body.String())
	}
}

func TestCORS_PreflightOPTIONS(t *testing.T) {
	c := &CORS{AllowedOrigins: []string{"http://localhost:5173"}}
	req := httptest.NewRequest(http.MethodOptions, "/admin/keys", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")

	// Wrap a handler that would fail the test if reached — preflight
	// must be terminated inside the middleware.
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("preflight leaked to next handler")
	})

	rec := httptest.NewRecorder()
	c.Middleware(failing).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("ACAO: want %q, got %q", "http://localhost:5173", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") || !strings.Contains(got, "OPTIONS") {
		t.Fatalf("ACAM: want POST+OPTIONS, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "Content-Type") {
		t.Fatalf("ACAH: want Authorization+Content-Type, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got != "300" {
		t.Fatalf("ACMA: want %q, got %q", "300", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC: want %q, got %q", "true", got)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("Vary: want to contain Origin, got %q", got)
	}
}

func TestCORS_Wildcard(t *testing.T) {
	c := &CORS{AllowedOrigins: []string{"*"}}
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "http://random.com")
	rec := httptest.NewRecorder()

	c.Middleware(nextOK()).ServeHTTP(rec, req)

	// We echo the concrete origin rather than "*" so a client that
	// later enables credentials keeps working.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://random.com" {
		t.Fatalf("ACAO: want %q, got %q", "http://random.com", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC: want %q, got %q", "true", got)
	}
}

func TestCORS_EmptyAllowlistPassThrough(t *testing.T) {
	c := &CORS{AllowedOrigins: nil}
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "http://x.com")
	rec := httptest.NewRecorder()

	c.Middleware(nextOK()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO: want empty (CORS disabled), got %q", got)
	}
	if got := rec.Header().Get("Vary"); got != "" {
		t.Fatalf("Vary: want empty (CORS disabled), got %q", got)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body: want next handler to run, got %q", rec.Body.String())
	}
}

func TestCORS_NoOriginHeader(t *testing.T) {
	c := &CORS{AllowedOrigins: []string{"http://localhost:5173"}}
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	// No Origin header at all: this mimics curl / server-to-server.
	rec := httptest.NewRecorder()

	c.Middleware(nextOK()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("ACAO: want empty (no Origin), got %q", got)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body: want next handler to run, got %q", rec.Body.String())
	}
}

func TestCORS_VaryOriginAlways(t *testing.T) {
	// When the middleware emits ACAO it must also emit Vary: Origin
	// so caches don't serve one origin's response to another.
	c := &CORS{AllowedOrigins: []string{"http://localhost:5173"}}
	req := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()

	c.Middleware(nextOK()).ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Fatalf("ACAO: want non-empty for allowed origin")
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("Vary: want to contain Origin, got %q", got)
	}
}
