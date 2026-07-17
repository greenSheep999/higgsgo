package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/observability"
)

// RoutePatternFn returns the chi route pattern (e.g. "/v1/jobs/{id}") for
// the given request, or the empty string when no pattern is available
// (typically 404s that never matched a route). Callers can inject their
// own function for tests; DefaultRoutePattern is the production one.
type RoutePatternFn func(*http.Request) string

// DefaultRoutePattern extracts the chi route pattern from the request
// context. Chi populates RouteContext during ServeHTTP so this only
// works when the caller has already been through the chi router.
func DefaultRoutePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		return rc.RoutePattern()
	}
	return ""
}

// metricsResponseWriter captures the status code so we can label the
// Prometheus counters after next.ServeHTTP returns. Handlers that never
// call WriteHeader are treated as 200 to match net/http semantics.
type metricsResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (m *metricsResponseWriter) WriteHeader(code int) {
	if !m.wroteHeader {
		m.status = code
		m.wroteHeader = true
	}
	m.ResponseWriter.WriteHeader(code)
}

func (m *metricsResponseWriter) Write(b []byte) (int, error) {
	if !m.wroteHeader {
		m.status = http.StatusOK
		m.wroteHeader = true
	}
	return m.ResponseWriter.Write(b)
}

// HTTPMetrics returns a chi-compatible middleware that records the request
// count, duration, and in-flight gauge into m. routePattern is the
// function used to derive the route label; pass DefaultRoutePattern in
// production. Nil m disables the middleware (returns next unchanged) so
// callers can hand in an optional pointer.
func HTTPMetrics(m *observability.Metrics, routePattern RoutePatternFn) func(http.Handler) http.Handler {
	if routePattern == nil {
		routePattern = DefaultRoutePattern
	}
	return func(next http.Handler) http.Handler {
		if m == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.HTTPInFlight.Inc()
			defer m.HTTPInFlight.Dec()

			rec := &metricsResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			route := routePattern(r)
			if route == "" {
				// Falls in here for anything that didn't match a chi
				// route (typical 404). Bucketing them all under
				// "unknown" keeps the cardinality bounded.
				route = "unknown"
			}
			status := strconv.Itoa(rec.status)
			elapsed := time.Since(start).Seconds()

			m.HTTPRequests.WithLabelValues(r.Method, route, status).Inc()
			m.HTTPDuration.WithLabelValues(r.Method, route).Observe(elapsed)
		})
	}
}
