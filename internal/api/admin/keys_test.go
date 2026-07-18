package admin

// Handler tests for the /admin/keys write-op surface (rotate / pause /
// resume / reset_usage). List / Create / Get / Revoke stay covered by
// integration in the CLI + sqlite tests — here we only guard the HTTP
// glue: routing, status codes, and response shape.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeKeysStore implements ports.APIKeyStore for KeysHandler tests. Only
// the write-op surface (Rotate/Pause/Resume/ResetMonthlyUsage/Revoke) is
// exercised by these tests; every other method panics so a silent handler
// dependency change is caught immediately.
type fakeKeysStore struct {
	rotatePlaintext string
	rotateErr       error
	rotateCalls     []string

	pauseErr   error
	pauseCalls []string

	resumeErr   error
	resumeCalls []string

	resetErr   error
	resetCalls []string
}

func (f *fakeKeysStore) Rotate(_ context.Context, id string) (string, error) {
	f.rotateCalls = append(f.rotateCalls, id)
	if f.rotateErr != nil {
		return "", f.rotateErr
	}
	return f.rotatePlaintext, nil
}
func (f *fakeKeysStore) Pause(_ context.Context, id string) error {
	f.pauseCalls = append(f.pauseCalls, id)
	return f.pauseErr
}
func (f *fakeKeysStore) Resume(_ context.Context, id string) error {
	f.resumeCalls = append(f.resumeCalls, id)
	return f.resumeErr
}
func (f *fakeKeysStore) ResetMonthlyUsage(_ context.Context, id string) error {
	f.resetCalls = append(f.resetCalls, id)
	return f.resetErr
}
func (f *fakeKeysStore) UpdateMeta(context.Context, string, ports.APIKeyMetaPatch) error {
	return nil
}

func (f *fakeKeysStore) UpdatePlaygroundScope(context.Context, string, domain.PlaygroundScope) error {
	panic("not implemented")
}

// Read/create surface is unused by these tests but must be present.
func (f *fakeKeysStore) Get(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (f *fakeKeysStore) GetByHash(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (f *fakeKeysStore) Create(context.Context, *domain.APIKey) error {
	panic("not implemented")
}
func (f *fakeKeysStore) Revoke(context.Context, string) error {
	panic("not implemented")
}
func (f *fakeKeysStore) IncrementUsage(context.Context, string, int64) error {
	panic("not implemented")
}
func (f *fakeKeysStore) List(context.Context) ([]domain.APIKey, error) {
	panic("not implemented")
}
func (f *fakeKeysStore) ListByCPAPartner(context.Context, string) ([]domain.APIKey, error) {
	panic("not implemented")
}

var _ ports.APIKeyStore = (*fakeKeysStore)(nil)

func newKeysRouter(store ports.APIKeyStore) chi.Router {
	r := chi.NewRouter()
	NewKeysHandler(store).Register(r)
	return r
}

func doJSON(t *testing.T, r chi.Router, method, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var body map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response for %s %s: %v (body=%q)", method, path, err, rec.Body.String())
		}
	}
	return rec.Code, body
}

func TestKeysHandler_Rotate(t *testing.T) {
	store := &fakeKeysStore{rotatePlaintext: "sk-hg-0123456789abcdef0123456789abcdef01234567"}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/key_rot/rotate")
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%v)", code, body)
	}
	if got, _ := body["id"].(string); got != "key_rot" {
		t.Errorf("id: got %q want key_rot", got)
	}
	got, _ := body["key"].(string)
	if !strings.HasPrefix(got, "sk-hg-") {
		t.Errorf("key: got %q want sk-hg-... prefix", got)
	}
	if len(store.rotateCalls) != 1 || store.rotateCalls[0] != "key_rot" {
		t.Errorf("rotate calls: got %v want [key_rot]", store.rotateCalls)
	}
}

func TestKeysHandler_RotateNotFound(t *testing.T) {
	store := &fakeKeysStore{rotateErr: domain.ErrAPIKeyNotFound}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/ghost/rotate")
	if code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (body=%v)", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["type"].(string); got != "not_found" {
		t.Errorf("error type: got %q want not_found", got)
	}
}

func TestKeysHandler_Pause(t *testing.T) {
	store := &fakeKeysStore{}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/key_p/pause")
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%v)", code, body)
	}
	if got, _ := body["status"].(string); got != domain.APIKeyStatusPaused {
		t.Errorf("status: got %q want %q", got, domain.APIKeyStatusPaused)
	}
	if len(store.pauseCalls) != 1 || store.pauseCalls[0] != "key_p" {
		t.Errorf("pause calls: got %v want [key_p]", store.pauseCalls)
	}
}

func TestKeysHandler_PauseRevokedConflict(t *testing.T) {
	store := &fakeKeysStore{pauseErr: domain.ErrAPIKeyRevoked}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/key_rev/pause")
	if code != http.StatusConflict {
		t.Fatalf("status: got %d want 409 (body=%v)", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["type"].(string); got != "api_key_revoked" {
		t.Errorf("error type: got %q want api_key_revoked", got)
	}
}

func TestKeysHandler_Resume(t *testing.T) {
	store := &fakeKeysStore{}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/key_r/resume")
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%v)", code, body)
	}
	if got, _ := body["status"].(string); got != domain.APIKeyStatusActive {
		t.Errorf("status: got %q want %q", got, domain.APIKeyStatusActive)
	}
	if len(store.resumeCalls) != 1 || store.resumeCalls[0] != "key_r" {
		t.Errorf("resume calls: got %v want [key_r]", store.resumeCalls)
	}
}

func TestKeysHandler_ResumeRevokedConflict(t *testing.T) {
	store := &fakeKeysStore{resumeErr: domain.ErrAPIKeyRevoked}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/key_r/resume")
	if code != http.StatusConflict {
		t.Fatalf("status: got %d want 409 (body=%v)", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["type"].(string); got != "api_key_revoked" {
		t.Errorf("error type: got %q want api_key_revoked", got)
	}
}

func TestKeysHandler_ResetUsage(t *testing.T) {
	store := &fakeKeysStore{}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/key_u/reset_usage")
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%v)", code, body)
	}
	// JSON numbers decode as float64 through encoding/json.
	if got, _ := body["monthly_used"].(float64); got != 0 {
		t.Errorf("monthly_used: got %v want 0", got)
	}
	if len(store.resetCalls) != 1 || store.resetCalls[0] != "key_u" {
		t.Errorf("reset calls: got %v want [key_u]", store.resetCalls)
	}
}

func TestKeysHandler_ResetUsageNotFound(t *testing.T) {
	store := &fakeKeysStore{resetErr: domain.ErrAPIKeyNotFound}
	r := newKeysRouter(store)

	code, body := doJSON(t, r, http.MethodPost, "/keys/ghost/reset_usage")
	if code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (body=%v)", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["type"].(string); got != "not_found" {
		t.Errorf("error type: got %q want not_found", got)
	}
}
