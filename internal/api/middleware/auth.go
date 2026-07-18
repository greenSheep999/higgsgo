// Package middleware holds HTTP middleware shared across the API surfaces.
package middleware

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// adminBearerCtxKey stashes a boolean marker on the request context when
// the caller authenticated with the deploy-wide admin bearer. Downstream
// handlers use IsAdminBearer to grant elevated scope (e.g. the playground
// gate treats admin callers as scope=full so the WebUI's admin login can
// reach every registered model without minting an API key).
type adminBearerCtxKey struct{}

// WithAdminBearer returns a copy of ctx flagged as an admin-bearer request.
// The flag is only ever set by BearerAuth on successful validation.
func WithAdminBearer(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminBearerCtxKey{}, true)
}

// IsAdminBearer reports whether ctx was flagged by BearerAuth as an
// admin-bearer request.
func IsAdminBearer(ctx context.Context) bool {
	v, _ := ctx.Value(adminBearerCtxKey{}).(bool)
	return v
}

// BearerAccepter is the interface BearerAuth compares each incoming
// bearer against. Small on purpose so the runtime-mutable admin
// bearer manager (internal/core/bearer.Manager) and the static
// string used by the CPA /internal/* surface can both plug in.
//
// Accepts returns true when candidate is currently authorized.
// Implementations must never accept an empty candidate: unauthenticated
// requests would otherwise slip through when a Manager is only
// half-configured.
type BearerAccepter interface {
	Accepts(candidate string) bool
}

// StaticBearer wraps a fixed secret as a BearerAccepter. Used by
// /internal/* which still reads the CPA plugin secret straight from
// TOML — that surface is not user-facing and does not need runtime
// mutation. Constant-time compare guards against timing attacks so
// call sites do not have to remember crypto/subtle.
type StaticBearer string

// Accepts implements BearerAccepter for a fixed secret. Empty
// candidates never match, and an empty StaticBearer means "server
// not configured" — the middleware treats that as a fatal 500 at
// request time.
func (s StaticBearer) Accepts(candidate string) bool {
	if s == "" || candidate == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(s)) == 1
}

// BearerAuth requires a matching "Authorization: Bearer <secret>" header.
// Used for /admin/* and /internal/* which each own a distinct secret.
// For /v1/* the api_key lookup lives in a dedicated middleware.
//
// The secret is checked via a BearerAccepter so /admin/* can hand in
// the runtime-mutable bearer.Manager (accepts both current and a
// short-lived previous during a rotation) while /internal/* keeps
// using StaticBearer over the TOML value.
//
// On success the request context is annotated via WithAdminBearer so a
// handler mounted behind BearerAuth can distinguish an admin caller from
// an API-key caller (the /v1/playground/* routes rely on this to grant
// full scope to the WebUI admin flow).
//
// A nil accepter — or a StaticBearer over an empty string — surfaces
// as a 500 "server not configured for auth"; the middleware refuses
// to fall open even if the operator forgets to wire the secret.
func BearerAuth(accepter BearerAccepter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if accepter == nil {
				http.Error(w, "server not configured for auth", http.StatusInternalServerError)
				return
			}
			h := r.Header.Get("Authorization")
			token := strings.TrimPrefix(h, "Bearer ")
			if token == h || !accepter.Accepts(token) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithAdminBearer(r.Context())))
		})
	}
}
