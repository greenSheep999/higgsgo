package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeAccountStore is a partial ports.AccountStore that only implements the
// methods AccountsHandler exercises. Every other method panics so that a
// silent behavior change in the handler surface is caught immediately.
type fakeAccountStore struct {
	// List behavior.
	listRows      []domain.Account
	listErr       error
	lastFilter    ports.AccountFilter
	listCallCount int

	// Get behavior.
	getResult *domain.Account
	getErr    error
	lastGetID string

	// MarkStatus behavior.
	markErr    error
	markCalls  []markStatusCall
	lastMarkID string

	// Upsert behavior (exercised by Import). upsertErr short-circuits the
	// call; otherwise the row is appended to upsertCalls so tests can
	// assert against what the handler wrote.
	upsertErr   error
	upsertCalls []domain.Account
}

type markStatusCall struct {
	ID     string
	Status domain.AccountStatus
	Reason string
}

func (f *fakeAccountStore) List(_ context.Context, filter ports.AccountFilter) ([]domain.Account, error) {
	f.lastFilter = filter
	f.listCallCount++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRows, nil
}

func (f *fakeAccountStore) Get(_ context.Context, id string) (*domain.Account, error) {
	f.lastGetID = id
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func (f *fakeAccountStore) MarkStatus(_ context.Context, id string, status domain.AccountStatus, reason string) error {
	f.lastMarkID = id
	f.markCalls = append(f.markCalls, markStatusCall{ID: id, Status: status, Reason: reason})
	return f.markErr
}

func (f *fakeAccountStore) Upsert(_ context.Context, a *domain.Account) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	// Store a copy so downstream mutation cannot poison the recorded call.
	f.upsertCalls = append(f.upsertCalls, *a)
	return nil
}

// Methods below are not used by any AccountsHandler flow; they panic to
// guarantee the test breaks loudly if a handler starts calling them without
// updates.

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

func (f *fakeAccountStore) MarkThrottled(context.Context, string, time.Time, string) error {
	return nil
}

func (f *fakeAccountStore) RecoverThrottled(context.Context) (int, error) { return 0, nil }

func (f *fakeAccountStore) IncrFailStreak(context.Context, string) (int, error) { return 0, nil }

func (f *fakeAccountStore) ResetFailStreak(context.Context, string) error { return nil }

func (f *fakeAccountStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	panic("not implemented")
}

func (f *fakeAccountStore) Unlock(context.Context, string, string) error {
	panic("not implemented")
}

func (f *fakeAccountStore) UpdateFreeQuota(context.Context, string, domain.FreeQuotaCounters) error {
	return nil
}

func (f *fakeAccountStore) ListUnlimActivations(context.Context, string) ([]domain.UnlimActivation, error) {
	return nil, nil
}

func (f *fakeAccountStore) ReplaceUnlimActivations(context.Context, string, []domain.UnlimActivation) error {
	return nil
}

func (f *fakeAccountStore) HasActiveUnlimFor(context.Context, string, string) (bool, error) {
	return false, nil
}

func (f *fakeAccountStore) CountActiveUnlimByJST(context.Context) (map[string]int, error) {
	return nil, nil
}

func (f *fakeAccountStore) UpdateUpstreamStatus(context.Context, string, ports.UpstreamStatusUpdate) error {
	return nil
}

func (f *fakeAccountStore) UpdateGraceStatus(context.Context, string, string) error {
	return nil
}

// newAccountsRouter builds a chi.Router with the AccountsHandler mounted so
// tests can hit the real routing surface end-to-end.
func newAccountsRouter(store ports.AccountStore) chi.Router {
	r := chi.NewRouter()
	h := NewAccountsHandler(store)
	h.Register(r)
	return r
}

// sensitiveKeys lists JSON keys that must never appear in an account view.
// If any of these leak into a response, upstream credentials could be
// reused by an attacker holding only a valid admin bearer token.
var sensitiveKeys = []string{
	"password",
	"password_enc",
	"cookies",
	"cookies_json",
	"session_id",
	"ua",
	"user_agent",
	"datadome_id",
	"datadome_client_id",
}

// assertNoSensitiveFields walks the JSON payload and fails the test if any
// well-known sensitive key is present at any depth.
func assertNoSensitiveFields(t *testing.T, payload any) {
	t.Helper()
	switch v := payload.(type) {
	case map[string]any:
		for k, child := range v {
			for _, banned := range sensitiveKeys {
				if k == banned {
					t.Errorf("sensitive key %q leaked in response body", k)
				}
			}
			assertNoSensitiveFields(t, child)
		}
	case []any:
		for _, child := range v {
			assertNoSensitiveFields(t, child)
		}
	}
}

func sampleAccounts() []domain.Account {
	return []domain.Account{
		{
			ID:                  "user_1",
			Email:               "a@example.com",
			Password:            "secret-pw-1",
			SessionID:           "sess_1",
			CookiesJSON:         `{"__session":"jwt"}`,
			UserAgent:           "Mozilla/5.0",
			DataDomeClientID:    "dd_1",
			WorkspaceID:         "ws_1",
			PlanType:            domain.PlanPlus,
			SubscriptionBalance: 12345,
			Status:              domain.StatusActive,
			RegisteredAt:        time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		},
		{
			ID:                  "user_2",
			Email:               "b@example.com",
			Password:            "secret-pw-2",
			SessionID:           "sess_2",
			CookiesJSON:         `{"__session":"jwt2"}`,
			UserAgent:           "Mozilla/5.0",
			DataDomeClientID:    "dd_2",
			WorkspaceID:         "ws_2",
			PlanType:            domain.PlanPro,
			SubscriptionBalance: 6789,
			Status:              domain.StatusActive,
			RegisteredAt:        time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
		},
	}
}

func TestAccountsHandler_List_ReturnsData(t *testing.T) {
	store := &fakeAccountStore{listRows: sampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rec.Body.String())
	}
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("body.data missing or wrong type: %T", body["data"])
	}
	if got, want := len(data), 2; got != want {
		t.Errorf("data len: got %d want %d", got, want)
	}
	assertNoSensitiveFields(t, body)
}

func TestAccountsHandler_List_PlanTypeFilter(t *testing.T) {
	store := &fakeAccountStore{listRows: sampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts?plan_type=plus", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if got, want := store.lastFilter.PlanType, domain.PlanType("plus"); got != want {
		t.Errorf("filter.PlanType: got %q want %q", got, want)
	}
	if store.listCallCount != 1 {
		t.Errorf("List calls: got %d want 1", store.listCallCount)
	}
}

func TestAccountsHandler_List_MinBalanceInvalid(t *testing.T) {
	store := &fakeAccountStore{listRows: sampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts?min_balance=abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.listCallCount != 0 {
		t.Errorf("List must not be called when min_balance is invalid; got calls=%d", store.listCallCount)
	}
}

func TestAccountsHandler_List_MinBalanceValid(t *testing.T) {
	store := &fakeAccountStore{listRows: sampleAccounts()}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts?min_balance=10000", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastFilter.MinBalance, int64(10000); got != want {
		t.Errorf("filter.MinBalance: got %d want %d", got, want)
	}
}

func TestAccountsHandler_Get_Success(t *testing.T) {
	rows := sampleAccounts()
	store := &fakeAccountStore{getResult: &rows[0]}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/user_1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.lastGetID != "user_1" {
		t.Errorf("lastGetID: got %q want %q", store.lastGetID, "user_1")
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["id"] != "user_1" {
		t.Errorf("id in body: got %v want user_1", body["id"])
	}
	assertNoSensitiveFields(t, body)
}

func TestAccountsHandler_Get_NotFound(t *testing.T) {
	store := &fakeAccountStore{getErr: domain.ErrAccountNotFound}
	r := newAccountsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/accounts/missing", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body.error missing or wrong type: %T", body["error"])
	}
	if errObj["type"] != "not_found" {
		t.Errorf("error.type: got %v want not_found", errObj["type"])
	}
	if _, hasMsg := errObj["message"]; !hasMsg {
		t.Errorf("error.message missing")
	}
}

func TestAccountsHandler_Pause_Resume_Delete(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus domain.AccountStatus
		wantReason string
	}{
		{"pause", http.MethodPost, "/accounts/user_1/pause", domain.StatusSuspended, "manual pause"},
		{"resume", http.MethodPost, "/accounts/user_1/resume", domain.StatusActive, "manual resume"},
		{"delete", http.MethodDelete, "/accounts/user_1", domain.StatusBanned, "manual delete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeAccountStore{}
			r := newAccountsRouter(store)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
			}
			if len(store.markCalls) != 1 {
				t.Fatalf("MarkStatus call count: got %d want 1", len(store.markCalls))
			}
			call := store.markCalls[0]
			if call.ID != "user_1" {
				t.Errorf("MarkStatus.ID: got %q want user_1", call.ID)
			}
			if call.Status != tc.wantStatus {
				t.Errorf("MarkStatus.Status: got %q want %q", call.Status, tc.wantStatus)
			}
			if call.Reason != tc.wantReason {
				t.Errorf("MarkStatus.Reason: got %q want %q", call.Reason, tc.wantReason)
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["status"] != string(tc.wantStatus) {
				t.Errorf("body.status: got %v want %q", body["status"], string(tc.wantStatus))
			}
			if body["id"] != "user_1" {
				t.Errorf("body.id: got %v want user_1", body["id"])
			}
		})
	}
}
