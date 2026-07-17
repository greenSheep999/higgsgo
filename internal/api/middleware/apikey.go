package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// apiKeyCtxKey is the context key under which we stash the resolved APIKey.
type apiKeyCtxKey struct{}

// APIKeyFromContext returns the caller's APIKey from a request context, or
// (nil, false) when the request did not carry a valid key.
func APIKeyFromContext(ctx context.Context) (*domain.APIKey, bool) {
	k, ok := ctx.Value(apiKeyCtxKey{}).(*domain.APIKey)
	return k, ok
}

// APIKeyAuth is the /v1 auth middleware. It expects:
//
//	Authorization: Bearer sk-hg-<40 hex chars>
//
// It looks the hashed key up in APIKeyStore, rejects revoked or unknown
// keys, and stashes the resolved APIKey in the request context.
//
// When `optional` is true the middleware allows requests without an
// Authorization header (context.Value returns nil). Used to keep GET
// /v1/models discoverable without a key in dev.
func APIKeyAuth(store ports.APIKeyStore, optional bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("Authorization")
			if raw == "" {
				if optional {
					next.ServeHTTP(w, r)
					return
				}
				writeAuthError(w, http.StatusUnauthorized, "missing_api_key",
					"Authorization: Bearer sk-hg-... required")
				return
			}
			token := strings.TrimPrefix(raw, "Bearer ")
			if token == raw {
				writeAuthError(w, http.StatusUnauthorized, "malformed_authorization",
					"Authorization header must start with 'Bearer '")
				return
			}
			hash, err := apikey.Parse(token)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "invalid_api_key", err.Error())
				return
			}
			// Short timeout — auth is on hot path.
			lookupCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			k, err := store.GetByHash(lookupCtx, hash)
			if err != nil {
				if errors.Is(err, domain.ErrAPIKeyNotFound) {
					writeAuthError(w, http.StatusUnauthorized, "invalid_api_key", "unknown key")
					return
				}
				writeAuthError(w, http.StatusInternalServerError, "auth_error", err.Error())
				return
			}
			if k.Status != "active" {
				writeAuthError(w, http.StatusForbidden, "api_key_revoked",
					"this API key has been revoked or disabled")
				return
			}
			if !k.HasBudget(0) {
				writeAuthError(w, http.StatusPaymentRequired, "quota_exhausted",
					"monthly quota exhausted for this API key")
				return
			}
			ctx := context.WithValue(r.Context(), apiKeyCtxKey{}, k)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeAuthError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    kind,
			"message": msg,
		},
	})
}
