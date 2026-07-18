package middleware

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// PlaygroundAuth is the /v1/playground/* auth middleware. Unlike APIKeyAuth
// it accepts two token shapes on the same Authorization header:
//
//   - The deploy-wide admin bearer (verified by constant-time compare
//     against `adminBearer`). On success the request context is flagged
//     via WithAdminBearer so PlaygroundGate and the handlers can grant
//     scope=full without minting an API key.
//   - An `sk-hg-...` API key (looked up in `store`). On success the
//     resolved APIKey is stashed via ContextWithAPIKey and the request
//     falls through to PlaygroundGate for the scope check.
//
// This lets the WebUI reuse whichever credential the operator is logged in
// with — admin bearer for the console, or an API key for the shared /v1
// surface — without splitting the playground into two mirror routes.
//
// A missing / malformed / unknown token yields a 401 with an
// error.type matching the client-side heuristics (`invalid_api_key`,
// `malformed_authorization`, etc.) so the WebUI's ApiError handling in
// @/lib/api reacts consistently across both credential paths.
func PlaygroundAuth(adminBearer string, store ports.APIKeyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("Authorization")
			if raw == "" {
				writeAuthError(w, http.StatusUnauthorized, "missing_api_key",
					"Authorization: Bearer <admin-bearer|sk-hg-...> required")
				return
			}
			token := strings.TrimPrefix(raw, "Bearer ")
			if token == raw {
				writeAuthError(w, http.StatusUnauthorized, "malformed_authorization",
					"Authorization header must start with 'Bearer '")
				return
			}
			// Admin bearer wins if configured and matches — no store lookup
			// so the console-side flow stays free of DB round-trips.
			if adminBearer != "" &&
				subtle.ConstantTimeCompare([]byte(token), []byte(adminBearer)) == 1 {
				next.ServeHTTP(w, r.WithContext(WithAdminBearer(r.Context())))
				return
			}
			// Fall through to the standard sk-hg- validation. Rejecting
			// anything that neither matches the admin bearer nor parses as
			// an API key keeps the middleware fail-closed.
			hash, err := apikey.Parse(token)
			if err != nil {
				writeAuthError(w, http.StatusUnauthorized, "invalid_api_key", err.Error())
				return
			}
			if store == nil {
				writeAuthError(w, http.StatusInternalServerError, "auth_error",
					"api key store not configured")
				return
			}
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
			switch k.Status {
			case domain.APIKeyStatusActive:
				// fall through
			case domain.APIKeyStatusPaused:
				writeAuthError(w, http.StatusUnauthorized, "api_key_paused",
					"this API key is paused")
				return
			case domain.APIKeyStatusRevoked:
				writeAuthError(w, http.StatusUnauthorized, "api_key_revoked",
					"this API key has been revoked")
				return
			default:
				writeAuthError(w, http.StatusUnauthorized, "api_key_disabled",
					"this API key is not in an active state")
				return
			}
			if !k.HasBudget(0) {
				writeAuthError(w, http.StatusPaymentRequired, "quota_exhausted",
					"monthly quota exhausted for this API key")
				return
			}
			ctx := ContextWithAPIKey(r.Context(), k)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// PlaygroundGate is the scope gate for /v1/playground/* routes. It admits
// callers in three ways:
//
//   - Admin bearer callers (IsAdminBearer=true) bypass the scope check and
//     fall through as if they held a scope=full key. Kept in lockstep with
//     the handler-level scope resolution so both middleware and handler
//     agree on the admin case.
//   - API-key callers with PlaygroundScope in {cheap, full} fall through.
//     Per-model scope checks live in the handler (they need spec cost).
//   - Everything else is rejected with 403 playground_disabled. That
//     includes scope=none, unrecognised scopes, missing keys, and empty
//     PlaygroundScope columns — all fail closed.
func PlaygroundGate() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsAdminBearer(r.Context()) {
				next.ServeHTTP(w, r)
				return
			}
			key, ok := APIKeyFromContext(r.Context())
			if !ok || key == nil {
				writePlaygroundError(w, http.StatusForbidden, "playground_disabled",
					"api key is not permitted to use the playground")
				return
			}
			scope := key.PlaygroundScope
			if scope == "" {
				scope = domain.PlaygroundScopeNone
			}
			if scope == domain.PlaygroundScopeNone {
				writePlaygroundError(w, http.StatusForbidden, "playground_disabled",
					"api key is not permitted to use the playground")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writePlaygroundError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    kind,
			"message": msg,
		},
	})
}
