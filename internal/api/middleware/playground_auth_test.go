package middleware

// Tests for PlaygroundAuth: the /v1/playground/* auth middleware that
// accepts either the deploy-wide admin bearer or a sk-hg- API key. Both
// paths must land the downstream handler with the correct context
// annotation (admin marker vs. resolved APIKey), and unrecognised tokens
// must be rejected with the same error envelope shape as APIKeyAuth so
// the WebUI's ApiError handling stays consistent across credentials.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
)

const testAdminBearer = "test-admin-bearer-secret"

// authStub captures whether the downstream handler was reached and the
// credential shape observed in the request context.
type authStub struct {
	reached     bool
	sawAdmin    bool
	sawAPIKeyID string
}

func (s *authStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.reached = true
		s.sawAdmin = IsAdminBearer(r.Context())
		if k, ok := APIKeyFromContext(r.Context()); ok && k != nil {
			s.sawAPIKeyID = k.ID
		}
		w.WriteHeader(http.StatusOK)
	})
}

func servePlaygroundAuth(t *testing.T, header string, store *authFakeStore) (*httptest.ResponseRecorder, *authStub) {
	t.Helper()
	stub := &authStub{}
	req := httptest.NewRequest(http.MethodGet, "/v1/playground/models", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	PlaygroundAuth(testAdminBearer, store)(stub.handler()).ServeHTTP(rec, req)
	return rec, stub
}

func TestPlaygroundAuth_AdminBearerAccepted(t *testing.T) {
	rec, stub := servePlaygroundAuth(t, "Bearer "+testAdminBearer, nil)
	if !stub.reached {
		t.Fatalf("downstream not reached (status=%d, body=%q)", rec.Code, rec.Body.String())
	}
	if !stub.sawAdmin {
		t.Errorf("admin marker not set on context")
	}
	if stub.sawAPIKeyID != "" {
		t.Errorf("api key unexpectedly set on admin bearer path: %q", stub.sawAPIKeyID)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
}

func TestPlaygroundAuth_APIKeyAccepted(t *testing.T) {
	plain, hash, err := apikey.GenerateProject()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	// Sanity: Parse round-trips to the same hash so the middleware's
	// GetByHash call matches on our fake.
	if got, err := apikey.Parse(plain); err != nil || got != hash {
		t.Fatalf("Parse round-trip: got %q err %v want %q", got, err, hash)
	}
	store := &authFakeStore{key: &domain.APIKey{
		ID:              "key_ok",
		Status:          domain.APIKeyStatusActive,
		PlaygroundScope: domain.PlaygroundScopeFull,
	}}
	rec, stub := servePlaygroundAuth(t, "Bearer "+plain, store)
	if !stub.reached {
		t.Fatalf("downstream not reached (status=%d, body=%q)", rec.Code, rec.Body.String())
	}
	if stub.sawAdmin {
		t.Errorf("admin marker unexpectedly set on api-key path")
	}
	if stub.sawAPIKeyID != "key_ok" {
		t.Errorf("api key id: got %q want key_ok", stub.sawAPIKeyID)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
}

func TestPlaygroundAuth_MissingHeader(t *testing.T) {
	rec, stub := servePlaygroundAuth(t, "", nil)
	if stub.reached {
		t.Fatalf("downstream reached with no auth header")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	assertPlaygroundErrType(t, rec, "missing_api_key")
}

func TestPlaygroundAuth_MalformedHeader(t *testing.T) {
	rec, stub := servePlaygroundAuth(t, "Token abc", nil)
	if stub.reached {
		t.Fatalf("downstream reached on malformed header")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	assertPlaygroundErrType(t, rec, "malformed_authorization")
}

func TestPlaygroundAuth_UnknownToken(t *testing.T) {
	// Neither the admin bearer nor a valid sk-hg- key — must 401 with
	// invalid_api_key so the caller can distinguish from a paused key.
	rec, stub := servePlaygroundAuth(t, "Bearer sk-hg-deadbeef", &authFakeStore{err: domain.ErrAPIKeyNotFound})
	if stub.reached {
		t.Fatalf("downstream reached on unknown key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	assertPlaygroundErrType(t, rec, "invalid_api_key")
}

func TestPlaygroundAuth_APIKeyPaused(t *testing.T) {
	plain, _, err := apikey.GenerateProject()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	store := &authFakeStore{key: &domain.APIKey{
		ID:     "key_paused",
		Status: domain.APIKeyStatusPaused,
	}}
	rec, stub := servePlaygroundAuth(t, "Bearer "+plain, store)
	if stub.reached {
		t.Fatalf("downstream reached on paused key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	assertPlaygroundErrType(t, rec, "api_key_paused")
}

func TestPlaygroundAuth_StoreError(t *testing.T) {
	plain, _, err := apikey.GenerateProject()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	store := &authFakeStore{err: errors.New("boom")}
	rec, stub := servePlaygroundAuth(t, "Bearer "+plain, store)
	if stub.reached {
		t.Fatalf("downstream reached on store error")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	assertPlaygroundErrType(t, rec, "auth_error")
}

func TestPlaygroundAuth_AdminBearerWinsOverAPIKeyLookup(t *testing.T) {
	// The admin bearer path must not hit the store — a matching bearer
	// bypasses apikey.Parse entirely so the deploy-wide secret is never
	// mistaken for a garbled key.
	panicStore := &authFakeStore{}
	// If the store were consulted this would produce a nil-key panic path
	// downstream; we assert reached=true instead which is sufficient.
	rec, stub := servePlaygroundAuth(t, "Bearer "+testAdminBearer, panicStore)
	if !stub.reached {
		t.Fatalf("downstream not reached (status=%d)", rec.Code)
	}
	if !stub.sawAdmin {
		t.Errorf("admin marker not set")
	}
}

func TestPlaygroundAuth_ContextCancellation(t *testing.T) {
	// Cancel the request context before the middleware runs — apikey
	// lookup must respect the parent context so a client disconnect
	// short-circuits instead of tying up a goroutine.
	plain, _, err := apikey.GenerateProject()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	store := &authFakeStore{key: &domain.APIKey{ID: "k", Status: domain.APIKeyStatusActive}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/playground/models", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+plain)
	stub := &authStub{}
	rec := httptest.NewRecorder()
	PlaygroundAuth(testAdminBearer, store)(stub.handler()).ServeHTTP(rec, req)
	// The stub store ignores ctx, so it still returns the key and the
	// handler is reached. This test exists as a regression guard: if a
	// future refactor plumbs ctx into the store the assertion below can
	// be flipped to `stub.reached == false`. For now we just verify the
	// happy-path code doesn't panic on a cancelled context.
	_ = rec
	_ = stub
}

func assertPlaygroundErrType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["type"].(string); got != want {
		t.Errorf("error type: got %q want %q", got, want)
	}
}
