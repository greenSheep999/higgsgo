package admin

// Tests for SettingsHandler: bearer metadata GET, rotate current-bearer
// verification, generate-vs-manual paths, validator error mapping, and
// the grace-window contract that lets the previous bearer keep passing
// auth for 30 seconds after a rotation.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/core/bearer"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// memSettingsStore mirrors the in-memory settings store used in the
// bearer package tests — kept package-local here so the admin tests
// stay independent of the sqlite adapter.
type memSettingsStore struct {
	mu   sync.Mutex
	data map[string]string
	ts   map[string]time.Time
}

func newMemSettingsStore() *memSettingsStore {
	return &memSettingsStore{data: map[string]string{}, ts: map[string]time.Time{}}
}

func (m *memSettingsStore) Get(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return "", domain.ErrSettingNotFound
	}
	return v, nil
}

func (m *memSettingsStore) Set(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	m.ts[key] = time.Now().UTC()
	return nil
}

func (m *memSettingsStore) UpdatedAt(_ context.Context, key string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.ts[key]
	if !ok {
		return time.Time{}, domain.ErrSettingNotFound
	}
	return t, nil
}

func newTestSettingsHandler(t *testing.T, tomlBearer string) (*SettingsHandler, *bearer.Manager, *memSettingsStore) {
	t.Helper()
	store := newMemSettingsStore()
	mgr := bearer.New(tomlBearer, store, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load manager: %v", err)
	}
	return NewSettingsHandler(mgr, store), mgr, store
}

// mountSettingsRouter wraps the handler in a chi router so URL params
// (n/a here) and the "/admin" prefix stripping match production paths.
func mountSettingsRouter(h *SettingsHandler) http.Handler {
	r := chi.NewRouter()
	r.Route("/admin", func(r chi.Router) { h.Register(r) })
	return r
}

func TestSettingsHandler_GetBearerReturnsMetadataOnlyOnFreshBoot(t *testing.T) {
	h, _, _ := newTestSettingsHandler(t, "dev-admin-token-abc")
	req := httptest.NewRequest(http.MethodGet, "/admin/settings/bearer", nil)
	rec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["source"] != "toml" {
		t.Errorf("source: got %v want toml", body["source"])
	}
	if body["last_4"] != "-abc" {
		t.Errorf("last_4: got %v want -abc", body["last_4"])
	}
	if _, hasBearer := body["new_bearer"]; hasBearer {
		t.Errorf("GET must never leak the plaintext bearer: body=%v", body)
	}
	if _, hasValue := body["value"]; hasValue {
		t.Errorf("GET must never leak the plaintext bearer: body=%v", body)
	}
}

func TestSettingsHandler_RotateRejectsMissingCurrentBearer(t *testing.T) {
	h, _, _ := newTestSettingsHandler(t, "dev-admin-token-abc")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(`{"new_bearer":"a-new-value-that-is-long-enough"}`))
	rec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if got := errType(body); got != "invalid_body" {
		t.Errorf("error.type: got %q want invalid_body", got)
	}
}

func TestSettingsHandler_RotateRejectsBadCurrentBearer(t *testing.T) {
	h, mgr, _ := newTestSettingsHandler(t, "dev-admin-token-abc")

	req := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(`{"current_bearer":"wrong-value","new_bearer":"a-new-value-that-is-long-enough"}`))
	rec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if got := errType(body); got != "invalid_current_bearer" {
		t.Errorf("error.type: got %q want invalid_current_bearer", got)
	}
	// Manager must be unchanged.
	if got := mgr.Current(); got != "dev-admin-token-abc" {
		t.Errorf("bearer changed after failed rotate: %q", got)
	}
}

func TestSettingsHandler_RotateManualHappyPath(t *testing.T) {
	h, mgr, store := newTestSettingsHandler(t, "dev-admin-token-abc")

	body := `{"current_bearer":"dev-admin-token-abc","new_bearer":"replacement-bearer-value-42"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var respBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if respBody["new_bearer"] != "replacement-bearer-value-42" {
		t.Errorf("new_bearer: got %v want replacement-bearer-value-42", respBody["new_bearer"])
	}
	if respBody["source"] != "db" {
		t.Errorf("source: got %v want db", respBody["source"])
	}
	if respBody["last_4"] != "e-42" {
		t.Errorf("last_4: got %v want e-42", respBody["last_4"])
	}
	if mgr.Current() != "replacement-bearer-value-42" {
		t.Errorf("manager not updated: got %q", mgr.Current())
	}
	// Store must have the new value persisted.
	if v, err := store.Get(context.Background(), bearer.SettingKey); err != nil || v != "replacement-bearer-value-42" {
		t.Errorf("store: got %q err %v want persisted new bearer", v, err)
	}
	// Grace window: the old bearer keeps passing Accepts for now.
	if !mgr.Accepts("dev-admin-token-abc") {
		t.Errorf("previous bearer rejected inside grace window; expected accept")
	}
}

func TestSettingsHandler_RotateGenerateFallback(t *testing.T) {
	h, mgr, _ := newTestSettingsHandler(t, "dev-admin-token-abc")

	body := `{"current_bearer":"dev-admin-token-abc"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var respBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gen, _ := respBody["new_bearer"].(string)
	if len(gen) != bearer.GeneratedBearerBytes*2 {
		t.Errorf("generated bearer length: got %d want %d", len(gen), bearer.GeneratedBearerBytes*2)
	}
	if mgr.Current() != gen {
		t.Errorf("manager not updated to generated bearer")
	}
}

func TestSettingsHandler_RotateSurfacesValidatorErrors(t *testing.T) {
	cases := []struct {
		name       string
		newBearer  string
		wantStatus int
		wantType   string
	}{
		{"too_short", "short", http.StatusBadRequest, "bearer_too_short"},
		{"whitespace", "has spaces in the value here", http.StatusBadRequest, "bearer_whitespace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, mgr, _ := newTestSettingsHandler(t, "dev-admin-token-abc")
			req := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
				bytes.NewBufferString(`{"current_bearer":"dev-admin-token-abc","new_bearer":"`+c.newBearer+`"}`))
			rec := httptest.NewRecorder()
			mountSettingsRouter(h).ServeHTTP(rec, req)

			if rec.Code != c.wantStatus {
				t.Fatalf("status: got %d want %d body=%s", rec.Code, c.wantStatus, rec.Body.String())
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if got := errType(body); got != c.wantType {
				t.Errorf("error.type: got %q want %q", got, c.wantType)
			}
			if mgr.Current() != "dev-admin-token-abc" {
				t.Errorf("bearer changed on validator failure: %q", mgr.Current())
			}
		})
	}
}

// TestSettingsHandler_BearerAuthGraceWindow drives BearerAuth end-to-end
// after a rotate to prove that the previous bearer keeps 200'ing during
// the grace window, and 401s once the deadline has passed. This is the
// core contract that makes the WebUI's rotate flow safe for in-flight
// XHRs.
func TestSettingsHandler_BearerAuthGraceWindow(t *testing.T) {
	h, mgr, _ := newTestSettingsHandler(t, "dev-admin-token-abc")

	// Rotate to a fresh value.
	body := `{"current_bearer":"dev-admin-token-abc","new_bearer":"post-rotate-bearer-value"}`
	rreq := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(body))
	rrec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rrec, rreq)
	if rrec.Code != http.StatusOK {
		t.Fatalf("rotate failed: %d body=%s", rrec.Code, rrec.Body.String())
	}

	// Mount a trivial protected endpoint behind BearerAuth(mgr).
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	protected := middleware.BearerAuth(mgr)(inner)

	call := func(bearerHdr string) int {
		req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
		req.Header.Set("Authorization", "Bearer "+bearerHdr)
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call("post-rotate-bearer-value"); got != http.StatusOK {
		t.Errorf("new bearer: got %d want 200", got)
	}
	if got := call("dev-admin-token-abc"); got != http.StatusOK {
		t.Errorf("old bearer inside grace: got %d want 200", got)
	}
	if got := call("something-unrelated"); got != http.StatusUnauthorized {
		t.Errorf("unrelated bearer: got %d want 401", got)
	}

	// Force the grace window to expire.
	forceExpireGrace(t, mgr)

	if got := call("dev-admin-token-abc"); got != http.StatusUnauthorized {
		t.Errorf("old bearer after grace: got %d want 401", got)
	}
	if got := call("post-rotate-bearer-value"); got != http.StatusOK {
		t.Errorf("new bearer after grace: got %d want 200", got)
	}
}

// forceExpireGrace reaches into the Manager to short-circuit the grace
// window without waiting 30 real seconds. Kept as a helper so if the
// internal representation changes we only fix it in one place.
func forceExpireGrace(t *testing.T, mgr *bearer.Manager) {
	t.Helper()
	// Rotate once more with the same value: it clears the previous
	// slot because the current == newBearer branch runs.
	if err := mgr.Rotate(context.Background(), mgr.Current()); err != nil {
		t.Fatalf("clear grace via no-op rotate: %v", err)
	}
}

func TestSettingsHandler_RotateWithBadJSON(t *testing.T) {
	h, _, _ := newTestSettingsHandler(t, "dev-admin-token-abc")
	req := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(`{not json`))
	rec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if got := errType(body); got != "invalid_body" {
		t.Errorf("error.type: got %q want invalid_body", got)
	}
}

// TestSettingsHandler_GetAfterRotateReportsDB proves that once the DB
// contains an override the source flips to "db" and updated_at is
// populated as an RFC3339 UTC string.
func TestSettingsHandler_GetAfterRotateReportsDB(t *testing.T) {
	h, _, _ := newTestSettingsHandler(t, "dev-admin-token-abc")

	rreq := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(`{"current_bearer":"dev-admin-token-abc","new_bearer":"post-rotate-value-ok"}`))
	rrec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rrec, rreq)
	if rrec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rrec.Code, rrec.Body.String())
	}

	greq := httptest.NewRequest(http.MethodGet, "/admin/settings/bearer", nil)
	grec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(grec, greq)
	if grec.Code != http.StatusOK {
		t.Fatalf("get: %d", grec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(grec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["source"] != "db" {
		t.Errorf("source: got %v want db", body["source"])
	}
	if body["last_4"] != "e-ok" {
		t.Errorf("last_4: got %v want e-ok", body["last_4"])
	}
	updated, _ := body["updated_at"].(string)
	if updated == "" {
		t.Errorf("updated_at should be populated when source=db")
	}
	if _, err := time.Parse(time.RFC3339, updated); err != nil {
		t.Errorf("updated_at %q is not RFC3339: %v", updated, err)
	}
}

// errType extracts the {error:{type:...}} envelope into a plain string
// so tests can compare against literal error type ids.
func errType(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	if e == nil {
		return ""
	}
	s, _ := e["type"].(string)
	return s
}

// TestSettingsHandler_RotateReturnsGenerateHex sanity-checks that the
// generated bearer is a valid lower-case hex string of the expected
// length. This is defensive — the same invariant is asserted in the
// bearer package tests — because a broken RNG would land silently
// otherwise.
func TestSettingsHandler_RotateReturnsGenerateHex(t *testing.T) {
	h, _, _ := newTestSettingsHandler(t, "dev-admin-token-abc")
	req := httptest.NewRequest(http.MethodPost, "/admin/settings/bearer/rotate",
		bytes.NewBufferString(`{"current_bearer":"dev-admin-token-abc"}`))
	rec := httptest.NewRecorder()
	mountSettingsRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	gen, _ := body["new_bearer"].(string)
	if !isLowerHex(gen) {
		t.Errorf("generated bearer %q is not lower-hex", gen)
	}
}

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// Guard: package-level compile-time assertion that our test store
// satisfies the ports.SettingsStore interface. Prevents a signature
// drift from silently disabling half the tests.
var _ = func() bool {
	var _ interface {
		Get(context.Context, string) (string, error)
		Set(context.Context, string, string) error
		UpdatedAt(context.Context, string) (time.Time, error)
	} = (*memSettingsStore)(nil)
	return true
}()
