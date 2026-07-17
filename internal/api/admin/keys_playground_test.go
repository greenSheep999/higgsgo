package admin

// Handler tests for the playground_scope write surface:
//
//   * POST /keys accepts a playground_scope field and forwards it to the
//     store's Create call so a fresh key can be minted with the correct
//     gate.
//   * POST /keys/{id}/playground_scope updates the column and echoes the
//     new value back.
//   * An invalid scope value on either path renders a 400 so a typo can't
//     silently downgrade the gate.
//   * Missing id on the update path returns a 404.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// pgFakeStore records Create + UpdatePlaygroundScope calls so tests can
// assert the handler forwarded the caller's scope value verbatim. Every
// other APIKeyStore method panics so a silent handler expansion is caught
// immediately.
type pgFakeStore struct {
	created     []domain.APIKey
	updateID    string
	updateScope domain.PlaygroundScope
	updateErr   error
	updateCalls int
}

func (f *pgFakeStore) Create(_ context.Context, k *domain.APIKey) error {
	f.created = append(f.created, *k)
	return nil
}
func (f *pgFakeStore) UpdatePlaygroundScope(_ context.Context, id string, scope domain.PlaygroundScope) error {
	f.updateCalls++
	f.updateID = id
	f.updateScope = scope
	return f.updateErr
}
func (f *pgFakeStore) Get(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (f *pgFakeStore) GetByHash(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (f *pgFakeStore) Revoke(context.Context, string) error { panic("not implemented") }
func (f *pgFakeStore) IncrementUsage(context.Context, string, int64) error {
	panic("not implemented")
}
func (f *pgFakeStore) List(context.Context) ([]domain.APIKey, error) {
	panic("not implemented")
}
func (f *pgFakeStore) ListByCPAPartner(context.Context, string) ([]domain.APIKey, error) {
	panic("not implemented")
}
func (f *pgFakeStore) Rotate(context.Context, string) (string, error) {
	panic("not implemented")
}
func (f *pgFakeStore) Pause(context.Context, string) error  { panic("not implemented") }
func (f *pgFakeStore) Resume(context.Context, string) error { panic("not implemented") }
func (f *pgFakeStore) ResetMonthlyUsage(context.Context, string) error {
	panic("not implemented")
}

var _ ports.APIKeyStore = (*pgFakeStore)(nil)

func newPGRouter(store ports.APIKeyStore) chi.Router {
	r := chi.NewRouter()
	NewKeysHandler(store).Register(r)
	return r
}

func doJSONWithBody(t *testing.T, r chi.Router, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

func TestKeysHandler_CreateWithPlaygroundScope(t *testing.T) {
	store := &pgFakeStore{}
	r := newPGRouter(store)

	code, body := doJSONWithBody(t, r, http.MethodPost, "/keys", map[string]any{
		"name":             "pg-test",
		"playground_scope": "cheap",
	})
	if code != http.StatusCreated {
		t.Fatalf("status: got %d want 201 (body=%v)", code, body)
	}
	if got, _ := body["playground_scope"].(string); got != "cheap" {
		t.Errorf("view.playground_scope: got %q want cheap", got)
	}
	if len(store.created) != 1 {
		t.Fatalf("created rows: got %d want 1", len(store.created))
	}
	if store.created[0].PlaygroundScope != domain.PlaygroundScopeCheap {
		t.Errorf("stored scope: got %q want %q",
			store.created[0].PlaygroundScope, domain.PlaygroundScopeCheap)
	}
}

func TestKeysHandler_CreateDefaultsScopeNone(t *testing.T) {
	store := &pgFakeStore{}
	r := newPGRouter(store)

	code, body := doJSONWithBody(t, r, http.MethodPost, "/keys", map[string]any{
		"name": "default",
	})
	if code != http.StatusCreated {
		t.Fatalf("status: got %d want 201 (body=%v)", code, body)
	}
	if got, _ := body["playground_scope"].(string); got != "none" {
		t.Errorf("default view scope: got %q want none", got)
	}
	if store.created[0].PlaygroundScope != domain.PlaygroundScopeNone {
		t.Errorf("stored default scope: got %q want %q",
			store.created[0].PlaygroundScope, domain.PlaygroundScopeNone)
	}
}

func TestKeysHandler_CreateInvalidScopeRejected(t *testing.T) {
	store := &pgFakeStore{}
	r := newPGRouter(store)

	code, body := doJSONWithBody(t, r, http.MethodPost, "/keys", map[string]any{
		"name":             "bad",
		"playground_scope": "everything",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%v)", code, body)
	}
	if len(store.created) != 0 {
		t.Errorf("expected no store Create call, got %d", len(store.created))
	}
}

func TestKeysHandler_UpdatePlaygroundScope(t *testing.T) {
	store := &pgFakeStore{}
	r := newPGRouter(store)

	code, body := doJSONWithBody(t, r, http.MethodPost, "/keys/key_pg/playground_scope",
		map[string]any{"scope": "full"})
	if code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%v)", code, body)
	}
	if got, _ := body["playground_scope"].(string); got != "full" {
		t.Errorf("echoed scope: got %q want full", got)
	}
	if store.updateCalls != 1 {
		t.Fatalf("update calls: got %d want 1", store.updateCalls)
	}
	if store.updateID != "key_pg" {
		t.Errorf("update id: got %q want key_pg", store.updateID)
	}
	if store.updateScope != domain.PlaygroundScopeFull {
		t.Errorf("update scope: got %q want %q",
			store.updateScope, domain.PlaygroundScopeFull)
	}
}

func TestKeysHandler_UpdatePlaygroundScopeInvalid(t *testing.T) {
	store := &pgFakeStore{}
	r := newPGRouter(store)

	code, body := doJSONWithBody(t, r, http.MethodPost, "/keys/key_pg/playground_scope",
		map[string]any{"scope": "everything"})
	if code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%v)", code, body)
	}
	if store.updateCalls != 0 {
		t.Errorf("expected no store call, got %d", store.updateCalls)
	}
}

func TestKeysHandler_UpdatePlaygroundScopeNotFound(t *testing.T) {
	store := &pgFakeStore{updateErr: domain.ErrAPIKeyNotFound}
	r := newPGRouter(store)

	code, body := doJSONWithBody(t, r, http.MethodPost, "/keys/ghost/playground_scope",
		map[string]any{"scope": "cheap"})
	if code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (body=%v)", code, body)
	}
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["type"].(string); got != "not_found" {
		t.Errorf("error type: got %q want not_found", got)
	}
}
