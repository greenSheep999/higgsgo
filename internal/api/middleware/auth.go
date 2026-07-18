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

// BearerAuth requires a matching "Authorization: Bearer <expected>" header.
// Used for /admin/* and /internal/* which share a single deploy-wide secret.
// For /v1/* the api_key lookup lives in a dedicated middleware.
//
// On success the request context is annotated via WithAdminBearer so a
// handler mounted behind BearerAuth can distinguish an admin caller from
// an API-key caller (the /v1/playground/* routes rely on this to grant
// full scope to the WebUI admin flow).
func BearerAuth(expected string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expected == "" {
				http.Error(w, "server not configured for auth", http.StatusInternalServerError)
				return
			}
			h := r.Header.Get("Authorization")
			token := strings.TrimPrefix(h, "Bearer ")
			if token == h || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithAdminBearer(r.Context())))
		})
	}
}
