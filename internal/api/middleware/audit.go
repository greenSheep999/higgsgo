package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// actorPrefixLen is how many leading chars of the bearer token we
// persist. Truncating avoids storing a full secret while still giving
// operators enough entropy to correlate multiple audit rows to the
// same caller.
const actorPrefixLen = 8

// anonymousActor is the sentinel written to audit_events.actor when
// the request had no Authorization: Bearer header. Distinguishing
// "no token" from "some prefix" keeps the actor field trivially
// groupable in downstream queries.
const anonymousActor = "anonymous"

// maxAuditBodyBytes caps how many bytes we read out of the request
// body for hashing. Admin write payloads are small (< 4 KiB in the
// existing handlers); the cap only exists so a pathological client
// cannot force the middleware to buffer megabytes into memory before
// hashing. The downstream handler still sees the full body — this
// cap only affects the bytes covered by body_hash.
const maxAuditBodyBytes = 1 << 20 // 1 MiB

// auditInsertTimeout bounds how long the background Insert may run
// before we give up. The middleware returns to the caller well before
// this timer fires; it only matters when the DB is wedged.
const auditInsertTimeout = 5 * time.Second

// auditResourceType maps a chi route pattern to the resource kind
// operators reason about in the audit UI. Routes not in the table
// still produce audit rows with an empty ResourceType; the field is
// intentionally best-effort so the middleware never drops a write.
//
// Extend this table alongside any new /admin write route.
var auditResourceType = map[string]string{
	// keys
	"/keys":      "apikey",
	"/keys/{id}": "apikey",
	// accounts
	"/accounts/{id}":        "account",
	"/accounts/{id}/pause":  "account",
	"/accounts/{id}/resume": "account",
	// groups (self + members + bindings)
	"/groups":                          "group",
	"/groups/{id}":                     "group",
	"/groups/{id}/members":             "group",
	"/groups/{id}/members/{accountId}": "group",
	"/groups/{id}/bindings":            "group",
	"/groups/{id}/bindings/{apiKeyId}": "group",
	// jobs
	"/jobs/purge": "job",
	"/jobs/{id}":  "job",
	// cpa partner (currently the internal listener carries its own
	// bearer; the entries stay cheap and future-proof a shared audit
	// chain).
	"/internal/register":                 "partner",
	"/internal/execute":                  "partner",
	"/internal/refresh_jwt/{partner_id}": "partner",
	"/internal/{partner_id}":             "partner",
}

// auditResourceParams lists which chi.URLParam names the middleware
// probes to resolve resource_id, in priority order. Routes tend to use
// {id} but a few use domain-specific names ({accountId}, {apiKeyId},
// {partner_id}); the first non-empty match wins.
var auditResourceParams = []string{"id", "partner_id", "accountId", "apiKeyId"}

// auditResponseWriter captures the response status code so Audit can
// attach it to the persisted row after the handler chain returns.
// Handlers that never call WriteHeader are treated as 200 to match
// net/http semantics.
type auditResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (a *auditResponseWriter) WriteHeader(code int) {
	if !a.wroteHeader {
		a.status = code
		a.wroteHeader = true
	}
	a.ResponseWriter.WriteHeader(code)
}

func (a *auditResponseWriter) Write(b []byte) (int, error) {
	if !a.wroteHeader {
		a.status = http.StatusOK
		a.wroteHeader = true
	}
	return a.ResponseWriter.Write(b)
}

// auditInsertFn is the signature the middleware calls to persist a
// row. In production it forwards to store.Insert; the test suite
// swaps it for a synchronous shim so goroutine scheduling does not
// leak into assertions.
type auditInsertFn func(ctx context.Context, e *domain.AuditEvent) error

// Audit returns a chi-compatible middleware that persists one row per
// admin write request (POST/PUT/PATCH/DELETE) into store. Reads
// (GET/HEAD/OPTIONS) are forwarded untouched — the audit table exists
// to trace mutations, not enumerate every list call.
//
// The middleware:
//   - drains and restores the request body so the downstream handler
//     still sees the same bytes;
//   - hashes the body with SHA-256 (hex) and stores only the hash — we
//     never persist secrets like API key names or group configs;
//   - truncates the bearer token to actorPrefixLen chars for the actor
//     field, falling back to "anonymous" when no bearer was sent;
//   - captures the chi RoutePattern and the first matching URLParam
//     to fill route / resource_type / resource_id.
//
// Insert runs in a background goroutine — a slow DB must never block
// the response. Errors are logged at warn but never propagated to the
// caller: an audit failure is an ops issue, not a client issue.
//
// A nil store returns a passthrough middleware so wiring code can
// pass an optional AuditStore without extra nil checks.
func Audit(store ports.AuditStore, logger *slog.Logger) func(http.Handler) http.Handler {
	if store == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return auditWith(store.Insert, logger)
}

// auditWith is the testable seam: it accepts the raw insert function
// so tests can pass a blocking shim that signals when the async row
// has been recorded. Production callers should use Audit.
func auditWith(insert auditInsertFn, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWriteMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			bodyHash := drainAndHashBody(r)

			rec := &auditResponseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			route := ""
			resourceID := ""
			if rc := chi.RouteContext(r.Context()); rc != nil {
				route = rc.RoutePattern()
				resourceID = firstNonEmptyURLParam(rc, auditResourceParams)
			}

			evt := &domain.AuditEvent{
				ID:           idgen.NewID("audit"),
				TS:           time.Now().UTC(),
				Actor:        actorFromRequest(r),
				Method:       r.Method,
				Path:         r.URL.Path,
				Route:        route,
				Status:       rec.status,
				ResourceType: resourceTypeForRoute(route),
				ResourceID:   resourceID,
				BodyHash:     bodyHash,
			}

			// Insert asynchronously so a slow DB or dropped
			// connection cannot delay the API response. We
			// deliberately detach from the request context: it
			// gets cancelled the moment the handler returns.
			go func(e *domain.AuditEvent) {
				ctx, cancel := context.WithTimeout(context.Background(), auditInsertTimeout)
				defer cancel()
				if err := insert(ctx, e); err != nil && logger != nil {
					logger.Warn("audit insert failed",
						slog.String("id", e.ID),
						slog.String("route", e.Route),
						slog.String("actor", e.Actor),
						slog.String("err", err.Error()),
					)
				}
			}(evt)
		})
	}
}

// isWriteMethod reports whether m is a mutating HTTP verb worth
// auditing. Anything else (GET/HEAD/OPTIONS/TRACE) skips the whole
// audit chain.
func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// drainAndHashBody reads r.Body into a bounded buffer, restores the
// body so the downstream handler still gets the full bytes, and
// returns the hex-encoded SHA-256 of what was read. Errors and empty
// bodies both surface as "" — the caller records the row regardless.
func drainAndHashBody(r *http.Request) string {
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	buf, err := io.ReadAll(io.LimitReader(r.Body, maxAuditBodyBytes))
	if err != nil {
		return ""
	}
	// Reinstate the body so the downstream handler sees exactly the
	// same bytes we hashed. Closing the original is safe: io.ReadAll
	// already exhausted the reader.
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(buf))
	if len(buf) == 0 {
		return ""
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// actorFromRequest returns the truncated bearer token, or
// anonymousActor when no Authorization: Bearer header is present.
// The full token is never returned or logged.
func actorFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return anonymousActor
	}
	token := strings.TrimPrefix(h, "Bearer ")
	if token == h || token == "" {
		return anonymousActor
	}
	if len(token) <= actorPrefixLen {
		return token
	}
	return token[:actorPrefixLen]
}

// firstNonEmptyURLParam walks names in priority order and returns
// the first non-empty chi.URLParam value found in rc. Empty when
// none of the candidates match — routes without an id (e.g.
// POST /admin/keys) fall through here.
func firstNonEmptyURLParam(rc *chi.Context, names []string) string {
	for _, name := range names {
		if v := rc.URLParam(name); v != "" {
			return v
		}
	}
	return ""
}

// resourceTypeForRoute maps a route pattern to the resource kind
// documented alongside auditResourceType. The middleware always
// records the row, even when the lookup misses — an unknown route
// simply yields an empty resource_type.
func resourceTypeForRoute(route string) string {
	if route == "" {
		return ""
	}
	if v, ok := auditResourceType[route]; ok {
		return v
	}
	return ""
}
