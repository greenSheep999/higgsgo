package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// skippedAccessLogPaths lists probe paths whose accesses are dropped from
// access logs to keep the signal-to-noise ratio high. Health checks and
// Prometheus scrapes hit these every few seconds; the metrics middleware
// still records them so we lose no observability.
var skippedAccessLogPaths = map[string]struct{}{
	"/health":  {},
	"/metrics": {},
}

// apiKeyIDLogPrefixLen is how many leading characters of an APIKey.ID
// we log. Truncating avoids leaking full identifiers into log storage
// while keeping enough entropy to correlate a request with a key row.
const apiKeyIDLogPrefixLen = 8

// accessLogResponseWriter captures the response status code and byte
// count so AccessLog can attach them to the log entry after the
// handler chain returns. Handlers that never call WriteHeader are
// treated as 200 to match net/http semantics.
type accessLogResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (a *accessLogResponseWriter) WriteHeader(code int) {
	if !a.wroteHeader {
		a.status = code
		a.wroteHeader = true
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *accessLogResponseWriter) Write(b []byte) (int, error) {
	if !a.wroteHeader {
		a.status = http.StatusOK
		a.wroteHeader = true
	}
	n, err := a.ResponseWriter.Write(b)
	a.bytes += n
	return n, err
}

// truncateAPIKeyID cuts an APIKey.ID to apiKeyIDLogPrefixLen chars so
// full ids never hit access logs. Shorter ids (mainly test fixtures)
// are returned unchanged.
func truncateAPIKeyID(id string) string {
	if len(id) <= apiKeyIDLogPrefixLen {
		return id
	}
	return id[:apiKeyIDLogPrefixLen]
}

// AccessLog emits a structured slog entry per HTTP request. It skips
// noisy probe paths (/health, /metrics) and enriches each entry with
// the chi request id, matched route pattern, and (when authenticated)
// a truncated api key id.
//
// Level policy: 5xx responses log at Warn so operators can grep for
// server errors; everything else logs at Info.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, skip := skippedAccessLogPaths[r.URL.Path]; skip {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			rec := &accessLogResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			route := ""
			if rc := chi.RouteContext(r.Context()); rc != nil {
				route = rc.RoutePattern()
			}

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", route),
				slog.Int("status", rec.status),
				slog.Int64("duration_ms", time.Since(start).Milliseconds()),
				slog.Int("bytes", rec.bytes),
			}
			if reqID := chimw.GetReqID(r.Context()); reqID != "" {
				attrs = append(attrs, slog.String("request_id", reqID))
			}
			if k, ok := APIKeyFromContext(r.Context()); ok && k != nil {
				attrs = append(attrs, slog.String("api_key_id", truncateAPIKeyID(k.ID)))
			}

			level := slog.LevelInfo
			if rec.status >= http.StatusInternalServerError {
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "access", attrs...)
		})
	}
}
