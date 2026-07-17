package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// accessLogEntry is the shape we unmarshal each JSON log line into.
// slog's JSON handler flattens attrs alongside the built-in keys, so
// a plain map is easier than modelling every attr as a field.
type accessLogEntry map[string]any

// newTestLogger returns a slog.Logger whose output is captured in the
// returned buffer, plus a helper that parses the buffer into one entry
// per emitted record. LevelDebug ensures nothing is silently filtered.
func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// parseLogEntries splits the captured buffer into individual JSON log
// lines and decodes each one. Empty lines (trailing newline) are skipped.
func parseLogEntries(t *testing.T, buf *bytes.Buffer) []accessLogEntry {
	t.Helper()
	var out []accessLogEntry
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var e accessLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// findAccessEntry returns the single log entry with msg="access" or
// fails the test. Useful when we expect exactly one access log.
func findAccessEntry(t *testing.T, entries []accessLogEntry) accessLogEntry {
	t.Helper()
	var hits []accessLogEntry
	for _, e := range entries {
		if e["msg"] == "access" {
			hits = append(hits, e)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 access entry, got %d (all=%v)", len(hits), entries)
	}
	return hits[0]
}

// newAccessLogRouter wires a chi router with AccessLog and a single
// route, matching the way server.go composes middleware.
func newAccessLogRouter(logger *slog.Logger, method, pattern string, handler http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Use(AccessLog(logger))
	r.Method(method, pattern, handler)
	return r
}

func TestAccessLog_LogsMethodPathStatus(t *testing.T) {
	logger, buf := newTestLogger()
	router := newAccessLogRouter(logger, http.MethodGet, "/foo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	e := findAccessEntry(t, parseLogEntries(t, buf))
	if got := e["method"]; got != "GET" {
		t.Errorf("method = %v, want GET", got)
	}
	if got := e["path"]; got != "/foo" {
		t.Errorf("path = %v, want /foo", got)
	}
	if got := e["route"]; got != "/foo" {
		t.Errorf("route = %v, want /foo", got)
	}
	// JSON numbers decode as float64.
	if got, _ := e["status"].(float64); got != 200 {
		t.Errorf("status = %v, want 200", e["status"])
	}
	if _, ok := e["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing or wrong type: %v", e["duration_ms"])
	}
	if got, _ := e["duration_ms"].(float64); got < 0 {
		t.Errorf("duration_ms = %v, want >= 0", got)
	}
	if e["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", e["level"])
	}
}

func TestAccessLog_SkipsHealth(t *testing.T) {
	logger, buf := newTestLogger()
	router := newAccessLogRouter(logger, http.MethodGet, "/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := buf.String(); got != "" {
		t.Errorf("expected no access log for /health, got %q", got)
	}
}

func TestAccessLog_SkipsMetrics(t *testing.T) {
	logger, buf := newTestLogger()
	router := newAccessLogRouter(logger, http.MethodGet, "/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := buf.String(); got != "" {
		t.Errorf("expected no access log for /metrics, got %q", got)
	}
}

func TestAccessLog_IncludesAPIKeyID(t *testing.T) {
	logger, buf := newTestLogger()
	// A tiny middleware that injects a fake APIKey the way APIKeyAuth
	// would in production. Placing it before AccessLog ensures the
	// key is visible when the access log entry is built.
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := &domain.APIKey{ID: "key_abcdef123"}
			next.ServeHTTP(w, r.WithContext(ContextWithAPIKey(r.Context(), k)))
		})
	}

	r := chi.NewRouter()
	r.Use(inject)
	r.Use(AccessLog(logger))
	r.Get("/foo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))

	e := findAccessEntry(t, parseLogEntries(t, buf))
	if got := e["api_key_id"]; got != "key_abcd" {
		t.Errorf("api_key_id = %v, want key_abcd (8-char truncate)", got)
	}
}

func TestAccessLog_IncludesRequestID(t *testing.T) {
	logger, buf := newTestLogger()
	r := chi.NewRouter()
	r.Use(chimw.RequestID) // must run first so AccessLog sees the id
	r.Use(AccessLog(logger))
	r.Get("/foo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))

	e := findAccessEntry(t, parseLogEntries(t, buf))
	got, ok := e["request_id"].(string)
	if !ok || got == "" {
		t.Errorf("request_id missing or empty: %v", e["request_id"])
	}
}

func TestAccessLog_ErrorStatusStillLogs(t *testing.T) {
	logger, buf := newTestLogger()
	router := newAccessLogRouter(logger, http.MethodGet, "/boom", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "kaboom", http.StatusInternalServerError)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	e := findAccessEntry(t, parseLogEntries(t, buf))
	if got, _ := e["status"].(float64); got != 500 {
		t.Errorf("status = %v, want 500", e["status"])
	}
}

func TestAccessLog_5xxAtWarnLevel(t *testing.T) {
	logger, buf := newTestLogger()
	router := newAccessLogRouter(logger, http.MethodGet, "/boom", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "kaboom", http.StatusBadGateway)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	e := findAccessEntry(t, parseLogEntries(t, buf))
	if e["level"] != "WARN" {
		t.Errorf("level = %v, want WARN for 5xx", e["level"])
	}
}

func TestAccessLog_TruncateAPIKeyID_Short(t *testing.T) {
	// Guardrail: ids shorter than the prefix length pass through
	// unchanged so short test fixtures don't get mangled.
	if got := truncateAPIKeyID("abc"); got != "abc" {
		t.Errorf("truncateAPIKeyID(abc) = %q, want abc", got)
	}
	if got := truncateAPIKeyID("key_abcdef123"); got != "key_abcd" {
		t.Errorf("truncateAPIKeyID(long) = %q, want key_abcd", got)
	}
}
