package middleware

import (
	"encoding/json"
	"net/http"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// PlaygroundGate is the gate for /v1/playground/* routes. It looks up the
// caller's APIKey in the request context (populated by APIKeyAuth) and
// rejects the request with a 403 playground_disabled when the key's
// PlaygroundScope is PlaygroundScopeNone. Cheap and full scopes fall
// through to the next handler which performs per-model scope checks.
//
// A missing APIKey in context is treated the same as scope=none: the
// wiring in server.go always mounts APIKeyAuth ahead of this middleware,
// so reaching PlaygroundGate without a key means the request slipped past
// auth — fail closed rather than open.
func PlaygroundGate() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
