package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/api/admin"
	"github.com/greensheep999/higgsgo/internal/api/cpaplugin"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeRegistrar is a minimal Registrar honouring the sentinel errors
// so the handler branches can be exercised without pulling in the
// full higgsfield adapter.
type fakeRegistrar struct {
	rows      []ports.RegistrationRow
	nextID    string
	enqErr    error
	getErr    error
	listErr   error
	retryErr  error
	notFound  bool
	enqCalled bool
}

func (f *fakeRegistrar) Enqueue(_ context.Context, _ ports.RegistrationRequest) (string, error) {
	f.enqCalled = true
	if f.enqErr != nil {
		return "", f.enqErr
	}
	if f.nextID == "" {
		return "reg_abc", nil
	}
	return f.nextID, nil
}

func (f *fakeRegistrar) GetStatus(_ context.Context, id string) (*ports.RegistrationRow, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.notFound {
		return nil, domain.ErrRegistrationNotFound
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			return &f.rows[i], nil
		}
	}
	return nil, domain.ErrRegistrationNotFound
}

func (f *fakeRegistrar) List(_ context.Context, _ ports.RegistrationFilter) ([]ports.RegistrationRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeRegistrar) Retry(_ context.Context, id string) error {
	if f.retryErr != nil {
		return f.retryErr
	}
	if f.notFound {
		return domain.ErrRegistrationNotFound
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			return nil
		}
	}
	return domain.ErrRegistrationNotFound
}

func mount(reg ports.Registrar) http.Handler {
	r := chi.NewRouter()
	admin.NewRegistrationsHandler(reg).Register(r)
	return r
}

func do(t *testing.T, h http.Handler, method, path string, body string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var envelope map[string]any
	if w.Body.Len() > 0 {
		if err := json.NewDecoder(w.Body).Decode(&envelope); err != nil {
			t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
		}
	}
	return w.Code, envelope
}

// TestNilRegistrar_All503 asserts every method on the handler
// answers 503 registrar_disabled when the Registrar is unset. This
// mirrors the shape the stub returns so the SPA can treat both
// cases identically.
func TestNilRegistrar_All503(t *testing.T) {
	t.Parallel()
	h := mount(nil)
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/registrations", ""},
		{"POST", "/registrations", `{"email":"a@b.co"}`},
		{"GET", "/registrations/reg_x", ""},
		{"POST", "/registrations/reg_x/retry", ""},
	} {
		code, env := do(t, h, tc.method, tc.path, tc.body)
		if code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: want 503, got %d (body=%v)", tc.method, tc.path, code, env)
		}
		errObj, _ := env["error"].(map[string]any)
		if errObj["type"] != "registrar_disabled" {
			t.Fatalf("%s %s: want type=registrar_disabled, got %v", tc.method, tc.path, errObj)
		}
	}
}

// TestStubRegistrar_All503 asserts the concrete stub Registrar
// implementation produces the same 503 envelope through the handler
// pipeline. This is the actually-configured path in slim builds.
func TestStubRegistrar_All503(t *testing.T) {
	t.Parallel()
	h := mount(cpaplugin.StubRegistrar{})
	code, env := do(t, h, "POST", "/registrations",
		`{"email":"a@b.co","password":"pw","mailbox_client_id":"cid","mailbox_refresh_token":"rt"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("stub Enqueue: want 503, got %d (env=%v)", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["type"] != "registrar_disabled" {
		t.Fatalf("stub Enqueue: want type=registrar_disabled, got %v", errObj)
	}
}

func TestEnqueue_MissingEmail_400(t *testing.T) {
	t.Parallel()
	h := mount(&fakeRegistrar{})
	code, env := do(t, h, "POST", "/registrations", `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (env=%v)", code, env)
	}
}

func TestEnqueue_HappyPath_202(t *testing.T) {
	t.Parallel()
	f := &fakeRegistrar{nextID: "reg_new"}
	h := mount(f)
	code, env := do(t, h, "POST", "/registrations",
		`{"email":"a@b.co","password":"pw","mailbox_client_id":"cid","mailbox_refresh_token":"rt"}`)
	if code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (env=%v)", code, env)
	}
	if env["id"] != "reg_new" || env["status"] != "pending" {
		t.Fatalf("unexpected envelope %v", env)
	}
	if !f.enqCalled {
		t.Fatalf("Enqueue not called")
	}
}

func TestGet_NotFound_404(t *testing.T) {
	t.Parallel()
	h := mount(&fakeRegistrar{notFound: true})
	code, env := do(t, h, "GET", "/registrations/reg_missing", "")
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d (env=%v)", code, env)
	}
}

func TestGet_HappyPath_200(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := &fakeRegistrar{rows: []ports.RegistrationRow{{
		ID:        "reg_ok",
		Email:     "a@b.co",
		Status:    "success",
		AccountID: "acc_1",
		CreatedAt: now,
	}}}
	h := mount(f)
	code, env := do(t, h, "GET", "/registrations/reg_ok", "")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d (env=%v)", code, env)
	}
	if env["id"] != "reg_ok" || env["status"] != "success" || env["account_id"] != "acc_1" {
		t.Fatalf("unexpected envelope %v", env)
	}
	if !strings.HasPrefix(env["created_at"].(string), "2026-01-02T03:04:05") {
		t.Fatalf("created_at wrong: %v", env["created_at"])
	}
}

func TestRetry_NotFound_404(t *testing.T) {
	t.Parallel()
	h := mount(&fakeRegistrar{notFound: true})
	code, _ := do(t, h, "POST", "/registrations/reg_missing/retry", "")
	if code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestList_HappyPath_200(t *testing.T) {
	t.Parallel()
	f := &fakeRegistrar{rows: []ports.RegistrationRow{{
		ID:     "reg_1",
		Email:  "one@x.co",
		Status: "pending",
	}}}
	h := mount(f)
	code, env := do(t, h, "GET", "/registrations?status=pending&limit=10&offset=0", "")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d (env=%v)", code, env)
	}
	data, ok := env["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("want 1 row, got %v", env["data"])
	}
}

// TestUnexpectedErrorPropagation asserts that a non-sentinel error
// from the underlying Registrar surfaces as 500 (not 503 or 404).
func TestUnexpectedErrorPropagation(t *testing.T) {
	t.Parallel()
	h := mount(&fakeRegistrar{enqErr: errors.New("boom")})
	code, env := do(t, h, "POST", "/registrations",
		`{"email":"a@b.co","password":"pw","mailbox_client_id":"cid","mailbox_refresh_token":"rt"}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d (env=%v)", code, env)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["type"] != "enqueue" {
		t.Fatalf("want type=enqueue, got %v", errObj)
	}
}

func TestEnqueue_PasswordFlow_MissingFields_400(t *testing.T) {
	t.Parallel()
	h := mount(&fakeRegistrar{})
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no password", `{"email":"a@b.co"}`, "password is required"},
		{"no mailbox creds", `{"email":"a@b.co","password":"pw"}`, "mailbox_client_id and mailbox_refresh_token"},
		{"partial mailbox", `{"email":"a@b.co","password":"pw","mailbox_client_id":"cid"}`, "mailbox_client_id and mailbox_refresh_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, env := do(t, h, "POST", "/registrations", tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (env=%v)", code, env)
			}
			errObj, _ := env["error"].(map[string]any)
			msg, _ := errObj["message"].(string)
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("want message containing %q, got %q", tc.want, msg)
			}
		})
	}
}

func TestEnqueue_OAuthFlow_NoPasswordRequired_202(t *testing.T) {
	t.Parallel()
	f := &fakeRegistrar{nextID: "reg_oauth"}
	h := mount(f)
	code, env := do(t, h, "POST", "/registrations",
		`{"email":"a@b.co","oauth_source":"google"}`)
	if code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (env=%v)", code, env)
	}
	if env["id"] != "reg_oauth" {
		t.Fatalf("unexpected envelope %v", env)
	}
}
