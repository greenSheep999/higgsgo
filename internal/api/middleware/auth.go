// Package middleware holds HTTP middleware shared across the API surfaces.
package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// BearerAuth requires a matching "Authorization: Bearer <expected>" header.
// Used for /admin/* and /internal/* which share a single deploy-wide secret.
// For /v1/* the api_key lookup lives in a dedicated middleware.
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
			next.ServeHTTP(w, r)
		})
	}
}
