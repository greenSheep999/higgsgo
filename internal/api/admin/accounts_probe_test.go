package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// stubProber lets each test steer FetchWallet's return value without
// pulling in the concrete upstream client.
type stubProber struct {
	wallet *ProbeWallet
	err    error
}

func (s *stubProber) FetchWallet(_ context.Context, _ *domain.Account) (*ProbeWallet, error) {
	return s.wallet, s.err
}

// probeTestAccount seeds the fakeAccountStore with one account so
// Probe's Get lookup finds it, then returns the id. fakeAccountStore
// serves whatever getResult it's told; each call to Get returns the
// same singleton so tests don't need to key by id.
func probeTestAccount(_ *testing.T, store *fakeAccountStore, status domain.AccountStatus) string {
	store.getResult = &domain.Account{
		ID:     "acc_probe",
		Email:  "probe@example.com",
		Status: status,
	}
	return store.getResult.ID
}

func newProbeRouter(t *testing.T, store *fakeAccountStore, prober Prober) http.Handler {
	t.Helper()
	h := &AccountsHandler{Accounts: store, Prober: prober}
	r := chi.NewRouter()
	h.Register(r)
	return r
}

// TestProbe_SuccessReportsBalanceAndLatency verifies the happy path:
// upstream returns a wallet, handler responds 200 with ok=true and the
// balance snapshot filled in. Latency is not asserted precisely, only
// that it's non-negative.
func TestProbe_SuccessReportsBalanceAndLatency(t *testing.T) {
	store := &fakeAccountStore{}
	id := probeTestAccount(t, store, domain.StatusActive)

	router := newProbeRouter(t, store, &stubProber{
		wallet: &ProbeWallet{WorkspaceID: "ws_x", SubscriptionBalance: 12345, CreditsBalance: 67},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/"+id+"/probe", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got ProbeResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK {
		t.Errorf("ok: got false want true (error=%+v)", got.Error)
	}
	if got.Balance == nil || got.Balance.WorkspaceID != "ws_x" {
		t.Errorf("balance: got %+v want ws_x/12345/67", got.Balance)
	}
	if got.LatencyMS < 0 {
		t.Errorf("latency_ms should be non-negative, got %d", got.LatencyMS)
	}
	if got.AccountID != id {
		t.Errorf("account_id: got %q want %q", got.AccountID, id)
	}
}

// TestProbe_ErrorReturnsStructuredBody confirms upstream failures are
// reported as 200 with ok=false + a classified Error, not as an HTTP
// error status. This lets the WebUI render probe failures inline
// alongside successful probes without special error handling.
func TestProbe_ErrorReturnsStructuredBody(t *testing.T) {
	store := &fakeAccountStore{}
	id := probeTestAccount(t, store, domain.StatusActive)

	cases := []struct {
		name    string
		err     error
		wantKind string
	}{
		{"unauthorized", domain.ErrUpstreamUnauthorized, "unauthorized"},
		{"forbidden", domain.ErrUpstreamForbidden, "forbidden"},
		{"rate_limit", domain.ErrUpstreamRateLimit, "rate_limit"},
		{"upstream_5xx", domain.ErrUpstreamServerError, "upstream_5xx"},
		{"timeout", domain.ErrUpstreamTimeout, "timeout"},
		{"context timeout", context.DeadlineExceeded, "timeout"},
		{"network", errors.New("dial tcp 10.0.0.1:1080: connection refused"), "network"},
		{"internal", errors.New("something else"), "internal"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			router := newProbeRouter(t, store, &stubProber{err: tc.err})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/accounts/"+id+"/probe", nil)
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			var got ProbeResponse
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.OK {
				t.Errorf("ok: got true want false")
			}
			if got.Error == nil || got.Error.Kind != tc.wantKind {
				t.Errorf("error.kind: got %+v want %q", got.Error, tc.wantKind)
			}
		})
	}
}

// TestProbe_NilProberReturns503 verifies the endpoint exists even when
// no upstream client is wired — a distinct signal to the WebUI so it
// can render a "probing not configured" state instead of pretending
// probes work.
func TestProbe_NilProberReturns503(t *testing.T) {
	store := &fakeAccountStore{}
	id := probeTestAccount(t, store, domain.StatusActive)

	// Handler with Prober left unset.
	h := &AccountsHandler{Accounts: store}
	r := chi.NewRouter()
	h.Register(r)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/"+id+"/probe", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503; body=%s", rec.Code, rec.Body.String())
	}
	// Sanity: body carries the distinctive type token so the frontend
	// can branch on it.
	if !bytes.Contains(rec.Body.Bytes(), []byte("probe_disabled")) {
		t.Errorf("body should mention probe_disabled: %s", rec.Body.String())
	}
}

// TestProbe_BannedAccountReturns409 guards against reactivating a
// deliberately-banned account via probing.
func TestProbe_BannedAccountReturns409(t *testing.T) {
	store := &fakeAccountStore{}
	id := probeTestAccount(t, store, domain.StatusBanned)

	router := newProbeRouter(t, store, &stubProber{
		wallet: &ProbeWallet{},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/"+id+"/probe", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409; body=%s", rec.Code, rec.Body.String())
	}
}
