package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/observability"
)

// Regression tests for the F5 hardening: /metrics used to live on the
// public listener with no auth, leaking route cardinality, error rates,
// and traffic patterns to any internet-facing caller. It now lives on
// the admin listener behind BearerAuth and is unmounted from the public
// listener entirely.

const testAdminBearerForMetrics = "test-admin-bearer-metrics"

// newMetricsTestServer builds a bare-bones Server with just enough wiring
// to exercise the /metrics route on both routers. All optional stores
// (V1, Accounts, Jobs, etc.) are intentionally nil — publicRouter and
// adminRouter guard on them, so leaving them off keeps the test focused.
func newMetricsTestServer(t *testing.T, metrics *observability.Metrics) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.AdminListen = "127.0.0.1:0"
	cfg.Server.AdminBearer = testAdminBearerForMetrics
	return &Server{
		Config:  cfg,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics,
	}
}

// TestPublicRouter_MetricsIsUnmounted asserts /metrics no longer answers
// on the public listener. Prometheus scrapers now target the admin
// listener; anyone still hitting the public one should see a 404.
func TestPublicRouter_MetricsIsUnmounted(t *testing.T) {
	m := observability.NewMetrics()
	s := newMetricsTestServer(t, m)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	s.publicRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("public /metrics status = %d, want 404 (route must not exist on public listener)", rec.Code)
	}
}

// TestAdminRouter_MetricsRejectsUnauthenticated asserts /metrics on the
// admin listener returns 401 without a bearer token — the same gate
// every other /admin/* route applies.
func TestAdminRouter_MetricsRejectsUnauthenticated(t *testing.T) {
	m := observability.NewMetrics()
	s := newMetricsTestServer(t, m)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	// No Authorization header at all.
	s.adminRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin /metrics without bearer = %d, want 401", rec.Code)
	}
}

// TestAdminRouter_MetricsRejectsWrongBearer asserts a wrong secret is
// rejected too (guards against a copy-paste of the /admin bearer path
// that accidentally weakened the check).
func TestAdminRouter_MetricsRejectsWrongBearer(t *testing.T) {
	m := observability.NewMetrics()
	s := newMetricsTestServer(t, m)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer definitely-not-the-secret")
	s.adminRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin /metrics with wrong bearer = %d, want 401", rec.Code)
	}
}

// TestAdminRouter_MetricsAllowsAdminBearer asserts the metrics endpoint
// serves Prometheus text exposition when the correct admin bearer is
// presented.
func TestAdminRouter_MetricsAllowsAdminBearer(t *testing.T) {
	m := observability.NewMetrics()
	s := newMetricsTestServer(t, m)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminBearerForMetrics)
	s.adminRouter().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("admin /metrics with correct bearer = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Prometheus text exposition always emits at least one HELP or TYPE
	// comment. The exact metric names are covered by
	// observability/metrics_test.go; here we only need to prove the
	// handler ran and returned real content.
	if !strings.Contains(body, "# HELP") && !strings.Contains(body, "# TYPE") {
		t.Fatalf("admin /metrics body does not look like Prometheus text exposition; got: %.200q", body)
	}
}

// TestAdminRouter_MetricsUnmountedWhenNil asserts the /metrics route is
// simply absent when Server.Metrics is nil — preserves the pre-existing
// "metrics off" code path so slim deployments that skip observability
// still boot cleanly. The admin router mounts the WebUI SPA as a
// NotFound fallback, so an unmounted /metrics falls through to that
// (never to the Prometheus handler). Assertion: whatever comes back,
// it is NOT Prometheus text exposition.
func TestAdminRouter_MetricsUnmountedWhenNil(t *testing.T) {
	s := newMetricsTestServer(t, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminBearerForMetrics)
	s.adminRouter().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "# HELP") || strings.Contains(body, "# TYPE") {
		t.Fatalf("admin /metrics with nil Metrics returned Prometheus exposition; the route must not be mounted. status=%d body=%.200q", rec.Code, body)
	}
}
