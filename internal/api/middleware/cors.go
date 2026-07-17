package middleware

import (
	"net/http"
	"strconv"
	"strings"
)

// CORS enforces a browser-facing CORS policy for the admin surface (and any
// other listener a WebUI needs to reach cross-origin). Instances are safe
// for concurrent use once constructed.
//
// Design notes:
//   - We deliberately echo the request Origin rather than emitting "*", so
//     that a caller that opts in later with credentials keeps working
//     without a middleware change.
//   - Requests that arrive without an Origin (curl, backend-to-backend
//     tools) are passed straight through so we do not accidentally break
//     non-browser clients. Same for requests whose Origin is not in the
//     allowlist: the browser will drop the response on its own and there
//     is no upside to returning 403 to a tool that ignored the header.
type CORS struct {
	// AllowedOrigins is the exact match list; use ["*"] for wildcard
	// (dev only). Empty list means CORS disabled — middleware becomes
	// a pass-through.
	AllowedOrigins []string
	// AllowedMethods defaults to the standard verb set when nil.
	AllowedMethods []string
	// AllowedHeaders defaults to "Authorization, Content-Type" when nil.
	AllowedHeaders []string
	// MaxAge preflight cache seconds. Default 300 (5min).
	MaxAge int
}

// defaultAllowedMethods matches the verbs the admin and public routers
// currently expose. Kept broad enough that adding a new admin route does
// not force a middleware config change.
var defaultAllowedMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodPatch,
	http.MethodDelete,
	http.MethodOptions,
}

var defaultAllowedHeaders = []string{"Authorization", "Content-Type"}

const defaultMaxAge = 300

// Middleware returns an http.Handler that applies the configured CORS
// policy in front of next.
func (c *CORS) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Empty allowlist == middleware disabled. Handing next the
		// request untouched keeps the code path identical to a server
		// that never mounted CORS at all.
		if len(c.AllowedOrigins) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" {
			// Non-browser client. Nothing to negotiate.
			next.ServeHTTP(w, r)
			return
		}

		if !c.originAllowed(origin) {
			// Unlisted origin: pass through without CORS headers.
			// A browser will refuse the response on its own; a
			// backend tool that ignored Origin is unaffected.
			next.ServeHTTP(w, r)
			return
		}

		// Vary must be set whenever the response depends on Origin,
		// even for the preflight branch, so intermediate caches do not
		// serve one origin's response to another.
		addVary(w, "Origin")
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", strings.Join(c.methods(), ", "))
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(c.headers(), ", "))
			w.Header().Set("Access-Control-Max-Age", c.maxAge())
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// originAllowed returns true when origin should receive CORS headers.
// A single "*" entry in the allowlist matches anything; otherwise an
// exact-string match is required.
func (c *CORS) originAllowed(origin string) bool {
	for _, allowed := range c.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}

func (c *CORS) methods() []string {
	if len(c.AllowedMethods) > 0 {
		return c.AllowedMethods
	}
	return defaultAllowedMethods
}

func (c *CORS) headers() []string {
	if len(c.AllowedHeaders) > 0 {
		return c.AllowedHeaders
	}
	return defaultAllowedHeaders
}

func (c *CORS) maxAge() string {
	age := c.MaxAge
	if age <= 0 {
		age = defaultMaxAge
	}
	return strconv.Itoa(age)
}

// addVary appends value to the existing Vary header, avoiding duplicates.
// http.Header.Add would also work but would repeat "Origin" if an upstream
// handler already set it.
func addVary(w http.ResponseWriter, value string) {
	existing := w.Header().Get("Vary")
	if existing == "" {
		w.Header().Set("Vary", value)
		return
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.TrimSpace(part) == value {
			return
		}
	}
	w.Header().Set("Vary", existing+", "+value)
}
