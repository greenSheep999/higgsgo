package middleware

// Tests for the /v1 auth middleware's status-check branch. Active keys
// must pass through; paused / revoked keys must be rejected with a
// 401 and a status-specific error type so clients can distinguish a
// temporary suspension from a permanent revocation.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// authFakeStore implements ports.APIKeyStore for the middleware only.
// Every path except GetByHash panics so a silent middleware regression
// (calling Get / Create / etc from the hot path) surfaces immediately.
type authFakeStore struct {
	key *domain.APIKey
	err error
}

func (s *authFakeStore) GetByHash(_ context.Context, _ string) (*domain.APIKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.key, nil
}
func (s *authFakeStore) Get(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (s *authFakeStore) Create(context.Context, *domain.APIKey) error {
	panic("not implemented")
}
func (s *authFakeStore) Revoke(context.Context, string) error { panic("not implemented") }
func (s *authFakeStore) IncrementUsage(context.Context, string, int64) error {
	panic("not implemented")
}
func (s *authFakeStore) List(context.Context) ([]domain.APIKey, error) {
	panic("not implemented")
}
func (s *authFakeStore) ListByCPAPartner(context.Context, string) ([]domain.APIKey, error) {
	panic("not implemented")
}
func (s *authFakeStore) Rotate(context.Context, string) (string, error) {
	panic("not implemented")
}
func (s *authFakeStore) Pause(context.Context, string) error  { panic("not implemented") }
func (s *authFakeStore) Resume(context.Context, string) error { panic("not implemented") }
func (s *authFakeStore) ResetMonthlyUsage(context.Context, string) error {
	panic("not implemented")
}
func (s *authFakeStore) UpdatePlaygroundScope(context.Context, string, domain.PlaygroundScope) error {
	panic("not implemented")
}

// bearerRequest builds a POST /v1/videos/generations with a valid
// Authorization header. The store side is what decides which APIKey
// row (if any) that header resolves to.
func bearerRequest(t *testing.T) *http.Request {
	t.Helper()
	pt, _, err := apikey.GenerateProject()
	if err != nil {
		t.Fatalf("mint plaintext: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", nil)
	r.Header.Set("Authorization", "Bearer "+pt)
	return r
}

func decodeAuthError(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var wrap struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		t.Fatalf("decode auth error: %v (body=%q)", err, body)
	}
	return wrap.Error.Type, wrap.Error.Message
}

func TestAPIKeyAuth_ActiveKeyAllowed(t *testing.T) {
	store := &authFakeStore{key: &domain.APIKey{
		ID:     "key_active",
		Status: domain.APIKeyStatusActive,
	}}
	called := false
	h := APIKeyAuth(store, false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(t))
	if !called {
		t.Fatalf("next handler never called (status=%d, body=%q)", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
}

func TestAPIKeyAuth_PausedKeyRejected(t *testing.T) {
	store := &authFakeStore{key: &domain.APIKey{
		ID:     "key_paused",
		Status: domain.APIKeyStatusPaused,
	}}
	called := false
	h := APIKeyAuth(store, false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(t))
	if called {
		t.Fatalf("next handler was called for a paused key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	kind, _ := decodeAuthError(t, rec.Body.Bytes())
	if kind != "api_key_paused" {
		t.Errorf("error type: got %q want api_key_paused", kind)
	}
}

func TestAPIKeyAuth_RevokedKeyRejected(t *testing.T) {
	store := &authFakeStore{key: &domain.APIKey{
		ID:     "key_revoked",
		Status: domain.APIKeyStatusRevoked,
	}}
	called := false
	h := APIKeyAuth(store, false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bearerRequest(t))
	if called {
		t.Fatalf("next handler was called for a revoked key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
	kind, _ := decodeAuthError(t, rec.Body.Bytes())
	if kind != "api_key_revoked" {
		t.Errorf("error type: got %q want api_key_revoked", kind)
	}
}

func (s *authFakeStore) UpdateMeta(context.Context, string, ports.APIKeyMetaPatch) error {
	return nil
}
