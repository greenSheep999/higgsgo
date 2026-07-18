package cpaplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// --- fakes ---------------------------------------------------------------

// fakeAPIKeyStore is a minimal ports.APIKeyStore for handler tests. Every
// method the handlers actually call is implemented; the rest panic so a
// silent widening of the handler surface is caught loudly.
type fakeAPIKeyStore struct {
	rows       []domain.APIKey
	createErr  error
	revokeErr  error
	listErr    error
	getErr     error
	created    []domain.APIKey
	revoked    []string
	nextGetKey *domain.APIKey
}

func (f *fakeAPIKeyStore) Get(_ context.Context, id string) (*domain.APIKey, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.nextGetKey != nil && f.nextGetKey.ID == id {
		return f.nextGetKey, nil
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			r := f.rows[i]
			return &r, nil
		}
	}
	return nil, domain.ErrAPIKeyNotFound
}

func (f *fakeAPIKeyStore) GetByHash(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}

func (f *fakeAPIKeyStore) Create(_ context.Context, k *domain.APIKey) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, *k)
	f.rows = append(f.rows, *k)
	return nil
}

func (f *fakeAPIKeyStore) Revoke(_ context.Context, id string) error {
	if f.revokeErr != nil {
		return f.revokeErr
	}
	f.revoked = append(f.revoked, id)
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows[i].Status = "revoked"
		}
	}
	return nil
}

func (f *fakeAPIKeyStore) IncrementUsage(context.Context, string, int64) error {
	panic("not implemented")
}

func (f *fakeAPIKeyStore) List(_ context.Context) ([]domain.APIKey, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]domain.APIKey, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

// ListByCPAPartner mirrors the sqlite store's behaviour: empty partnerID
// returns an empty slice, otherwise filter rows by CPAPartnerID. Order
// intentionally matches insertion order for deterministic assertions.
func (f *fakeAPIKeyStore) ListByCPAPartner(_ context.Context, partnerID string) ([]domain.APIKey, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if partnerID == "" {
		return nil, nil
	}
	out := make([]domain.APIKey, 0, len(f.rows))
	for i := range f.rows {
		if f.rows[i].CPAPartnerID == partnerID {
			out = append(out, f.rows[i])
		}
	}
	return out, nil
}

// The write-op surfaces below are not exercised by the CPA plugin
// handler tests; they exist purely to satisfy the ports.APIKeyStore
// interface. Any handler that starts calling them without updating this
// stub will panic loudly.
func (f *fakeAPIKeyStore) Rotate(context.Context, string) (string, error) {
	panic("not implemented")
}
func (f *fakeAPIKeyStore) Pause(context.Context, string) error  { panic("not implemented") }
func (f *fakeAPIKeyStore) Resume(context.Context, string) error { panic("not implemented") }
func (f *fakeAPIKeyStore) ResetMonthlyUsage(context.Context, string) error {
	panic("not implemented")
}
func (f *fakeAPIKeyStore) UpdatePlaygroundScope(context.Context, string, domain.PlaygroundScope) error {
	panic("not implemented")
}

// fakeAccountStore only implements List (used by refresh_jwt + status).
type fakeAccountStore struct {
	rows    []domain.Account
	listErr error
}

func (f *fakeAccountStore) List(_ context.Context, _ ports.AccountFilter) ([]domain.Account, error) {
	return f.rows, f.listErr
}

func (f *fakeAccountStore) Get(context.Context, string) (*domain.Account, error) {
	panic("not implemented")
}
func (f *fakeAccountStore) Upsert(context.Context, *domain.Account) error {
	panic("not implemented")
}
func (f *fakeAccountStore) UpdateBalance(context.Context, string, int64, int64, int64) error {
	panic("not implemented")
}
func (f *fakeAccountStore) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	panic("not implemented")
}
func (f *fakeAccountStore) UpdateInFlight(context.Context, string, int) error {
	panic("not implemented")
}
func (f *fakeAccountStore) ResetAllInFlight(context.Context) (int, error) { return 0, nil }
func (f *fakeAccountStore) MarkStatus(context.Context, string, domain.AccountStatus, string) error {
	panic("not implemented")
}
func (f *fakeAccountStore) MarkThrottled(context.Context, string, time.Time, string) error {
	panic("not implemented")
}
func (f *fakeAccountStore) RecoverThrottled(context.Context) (int, error) { return 0, nil }
func (f *fakeAccountStore) IncrFailStreak(context.Context, string) (int, error) {
	panic("not implemented")
}
func (f *fakeAccountStore) ResetFailStreak(context.Context, string) error {
	panic("not implemented")
}
func (f *fakeAccountStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	panic("not implemented")
}
func (f *fakeAccountStore) Unlock(context.Context, string, string) error {
	panic("not implemented")
}

// fakeJobStore is unused by the handlers under test but ports.JobStore is
// part of the Handler struct so it must at least type-check.
type fakeJobStore struct{}

func (f *fakeJobStore) Create(context.Context, *domain.Job) error { return nil }
func (f *fakeJobStore) UpdateStatus(context.Context, string, domain.JobStatus, ports.JobMeta) error {
	return nil
}
func (f *fakeJobStore) Get(context.Context, string) (*domain.Job, error) {
	return nil, domain.ErrJobNotFound
}
func (f *fakeJobStore) ListPending(context.Context) ([]domain.Job, error) { return nil, nil }
func (f *fakeJobStore) ListByAPIKey(context.Context, string, ports.JobFilter) ([]domain.Job, error) {
	return nil, nil
}
func (f *fakeJobStore) ListAll(context.Context, ports.JobFilter) ([]domain.Job, error) {
	return nil, nil
}
func (f *fakeJobStore) Purge(context.Context, time.Time, []domain.JobStatus) (int, error) {
	return 0, nil
}

// fakeProxy records the last request and returns a canned response.
type fakeProxy struct {
	lastReq proxy.GenerationRequest
	resp    *proxy.GenerationResponse
	err     error
	calls   int
}

func (f *fakeProxy) Generate(_ context.Context, req proxy.GenerationRequest) (*proxy.GenerationResponse, error) {
	f.lastReq = req
	f.calls++
	return f.resp, f.err
}

// fakeJWT records Invalidate calls.
type fakeJWT struct{ invalidated []string }

func (f *fakeJWT) Invalidate(id string) { f.invalidated = append(f.invalidated, id) }

// newTestHandler wires up a Handler over the given fakes and returns a
// chi router with every route mounted (no bearer auth — the tests exercise
// the routes directly).
func newTestHandler(t *testing.T, apiKeys *fakeAPIKeyStore, accounts *fakeAccountStore, invoker ProxyInvoker, minter JWTMinter) (*Handler, http.Handler) {
	t.Helper()
	h := New(apiKeys, accounts, &fakeJobStore{}, invoker, minter, nil)
	r := chi.NewRouter()
	h.Register(r)
	return h, r
}

// doJSON is a small helper for sending a JSON body to the router.
func doJSON(t *testing.T, r http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	b, err := io.ReadAll(rr.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal body %q: %v", b, err)
	}
	return m
}

// --- tests ---------------------------------------------------------------

func TestRegister_Success(t *testing.T) {
	apiKeys := &fakeAPIKeyStore{}
	_, r := newTestHandler(t, apiKeys, &fakeAccountStore{}, &fakeProxy{}, &fakeJWT{})

	body := map[string]any{
		"partner_id":    "cpa_xyz",
		"email":         "ops@example.com",
		"markup_pct":    1.2,
		"monthly_limit": 100000,
	}
	rr := doJSON(t, r, http.MethodPost, "/internal/register", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	if len(apiKeys.created) != 1 {
		t.Fatalf("expected 1 create, got %d", len(apiKeys.created))
	}
	k := apiKeys.created[0]
	if k.CPAPartnerID != "cpa_xyz" {
		t.Errorf("CPAPartnerID = %q, want cpa_xyz", k.CPAPartnerID)
	}
	if k.CreatedBy != cpaRegisterCreatedBy {
		t.Errorf("CreatedBy = %q, want %q", k.CreatedBy, cpaRegisterCreatedBy)
	}
	if k.MarkupPct != 1.2 {
		t.Errorf("MarkupPct = %v, want 1.2", k.MarkupPct)
	}
	if k.MonthlyQuota != 100000 {
		t.Errorf("MonthlyQuota = %d, want 100000", k.MonthlyQuota)
	}
	out := decodeBody(t, rr)
	if out["cpa_partner_id"] != "cpa_xyz" {
		t.Errorf("response cpa_partner_id = %v", out["cpa_partner_id"])
	}
	if key, _ := out["key"].(string); key == "" {
		t.Errorf("response missing plaintext key")
	}
}

func TestRegister_BadRequest(t *testing.T) {
	apiKeys := &fakeAPIKeyStore{}
	_, r := newTestHandler(t, apiKeys, &fakeAccountStore{}, &fakeProxy{}, &fakeJWT{})

	rr := doJSON(t, r, http.MethodPost, "/internal/register", map[string]any{"email": "x@y"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if len(apiKeys.created) != 0 {
		t.Errorf("no key should have been created; got %d", len(apiKeys.created))
	}
}

func TestExecute_Success(t *testing.T) {
	apiKeys := &fakeAPIKeyStore{
		rows: []domain.APIKey{{
			ID: "key-1", CPAPartnerID: "partner_a", CreatedBy: cpaRegisterCreatedBy, Status: "active",
		}},
	}
	fp := &fakeProxy{resp: &proxy.GenerationResponse{
		ID: "job-1", Status: "completed", Model: "seedance-2-0-mini",
	}}
	_, r := newTestHandler(t, apiKeys, &fakeAccountStore{}, fp, &fakeJWT{})

	body := map[string]any{
		"cpa_partner_id": "partner_a",
		"model":          "seedance-2-0-mini",
		"prompt":         "a red apple",
	}
	rr := doJSON(t, r, http.MethodPost, "/internal/execute", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if fp.calls != 1 {
		t.Fatalf("proxy invoked %d times, want 1", fp.calls)
	}
	if fp.lastReq.APIKeyID != "key-1" {
		t.Errorf("APIKeyID = %q, want key-1", fp.lastReq.APIKeyID)
	}
	if fp.lastReq.CPAPartnerID != "partner_a" {
		t.Errorf("CPAPartnerID = %q, want partner_a", fp.lastReq.CPAPartnerID)
	}
	if got := fp.lastReq.UserParams["prompt"]; got != "a red apple" {
		t.Errorf("UserParams[prompt] = %v, want %q", got, "a red apple")
	}
	out := decodeBody(t, rr)
	if out["id"] != "job-1" || out["status"] != "completed" {
		t.Errorf("response body mismatch: %v", out)
	}
}

func TestExecute_ProxyError(t *testing.T) {
	apiKeys := &fakeAPIKeyStore{
		rows: []domain.APIKey{{ID: "key-1", CPAPartnerID: "partner_a", CreatedBy: cpaRegisterCreatedBy, Status: "active"}},
	}
	fp := &fakeProxy{err: errors.New("upstream boom")}
	_, r := newTestHandler(t, apiKeys, &fakeAccountStore{}, fp, &fakeJWT{})

	body := map[string]any{"cpa_partner_id": "partner_a", "model": "m"}
	rr := doJSON(t, r, http.MethodPost, "/internal/execute", body)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rr.Code, rr.Body.String())
	}
	out := decodeBody(t, rr)
	if _, ok := out["error"]; !ok {
		t.Errorf("expected error envelope, got %v", out)
	}
}

func TestBalance_AggregatesKeys(t *testing.T) {
	apiKeys := &fakeAPIKeyStore{
		rows: []domain.APIKey{
			{ID: "k1", CPAPartnerID: "partner_x", CreatedBy: cpaRegisterCreatedBy, Status: "active", MonthlyUsed: 100, MonthlyQuota: 1000},
			{ID: "k2", CPAPartnerID: "partner_x", CreatedBy: cpaRegisterCreatedBy, Status: "active", MonthlyUsed: 200, MonthlyQuota: 2000},
			{ID: "k3", CPAPartnerID: "partner_x", CreatedBy: cpaRegisterCreatedBy, Status: "active", MonthlyUsed: 300, MonthlyQuota: 3000},
			// Non-matching row to ensure filtering works.
			{ID: "k4", CPAPartnerID: "other", CreatedBy: cpaRegisterCreatedBy, Status: "active", MonthlyUsed: 999, MonthlyQuota: 1000},
		},
	}
	_, r := newTestHandler(t, apiKeys, &fakeAccountStore{}, &fakeProxy{}, &fakeJWT{})

	rr := doJSON(t, r, http.MethodGet, "/internal/balance/partner_x", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	out := decodeBody(t, rr)
	if out["partner_id"] != "partner_x" {
		t.Errorf("partner_id = %v", out["partner_id"])
	}
	// JSON numbers unmarshal as float64.
	if got := out["total_used_h"].(float64); got != 600 {
		t.Errorf("total_used_h = %v, want 600", got)
	}
	if got := out["total_limit_h"].(float64); got != 6000 {
		t.Errorf("total_limit_h = %v, want 6000", got)
	}
	if got := out["key_count"].(float64); got != 3 {
		t.Errorf("key_count = %v, want 3", got)
	}
}

func TestRefreshJWT_InvalidatesAll(t *testing.T) {
	apiKeys := &fakeAPIKeyStore{
		rows: []domain.APIKey{{ID: "k1", CPAPartnerID: "partner_a", CreatedBy: cpaRegisterCreatedBy, Status: "active"}},
	}
	accs := &fakeAccountStore{
		rows: []domain.Account{
			{ID: "acc-1"}, {ID: "acc-2"}, {ID: "acc-3"},
		},
	}
	jwt := &fakeJWT{}
	_, r := newTestHandler(t, apiKeys, accs, &fakeProxy{}, jwt)

	rr := doJSON(t, r, http.MethodPost, "/internal/refresh_jwt/partner_a", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := len(jwt.invalidated); got != 3 {
		t.Fatalf("Invalidate called %d times, want 3", got)
	}
	out := decodeBody(t, rr)
	if got := out["invalidated"].(float64); got != 3 {
		t.Errorf("invalidated = %v, want 3", got)
	}
}

func TestDelete_DisablesAllKeys(t *testing.T) {
	apiKeys := &fakeAPIKeyStore{
		rows: []domain.APIKey{
			{ID: "k1", CPAPartnerID: "partner_a", CreatedBy: cpaRegisterCreatedBy, Status: "active"},
			{ID: "k2", CPAPartnerID: "partner_a", CreatedBy: cpaRegisterCreatedBy, Status: "active"},
			{ID: "k3", CPAPartnerID: "other", CreatedBy: cpaRegisterCreatedBy, Status: "active"},
		},
	}
	_, r := newTestHandler(t, apiKeys, &fakeAccountStore{}, &fakeProxy{}, &fakeJWT{})

	rr := doJSON(t, r, http.MethodDelete, "/internal/partner_a", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := len(apiKeys.revoked); got != 2 {
		t.Fatalf("Revoke called %d times, want 2", got)
	}
	// Make sure the "other" partner's key was NOT touched.
	for _, id := range apiKeys.revoked {
		if id == "k3" {
			t.Errorf("Revoke touched foreign partner key %s", id)
		}
	}
}

func TestRegistrations_501(t *testing.T) {
	_, r := newTestHandler(t, &fakeAPIKeyStore{}, &fakeAccountStore{}, &fakeProxy{}, &fakeJWT{})

	rr := doJSON(t, r, http.MethodGet, "/internal/registrations/foo", nil)
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rr.Code)
	}
	out := decodeBody(t, rr)
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %v", out)
	}
	if errObj["type"] != "not_implemented" {
		t.Errorf("error.type = %v", errObj["type"])
	}
}

func (s *fakeAPIKeyStore) UpdateMeta(context.Context, string, ports.APIKeyMetaPatch) error {
	return nil
}
