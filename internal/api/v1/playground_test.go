package v1

// Handler tests for /v1/playground/*:
//
//   * /models filters by PlaygroundScope: cheap only sees under-cap
//     entries as allowed, full sees everything, none is treated as no
//     model allowed (the middleware normally blocks such calls before
//     they reach the handler, but /models must still fail closed on the
//     defensive path).
//   * /estimate returns cost and gate flags for allowed models and
//     rejects blocked models with 403 blocked_by_scope.
//   * /execute forwards to HandleImageGeneration or HandleVideoGeneration
//     based on spec.Output after the same per-model scope check.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// playgroundSpecs builds a hand-crafted registry that spans the cheap
// threshold and both Output branches so /models can be asserted per-scope
// and /execute has a spec of each Output type to hit.
func playgroundSpecs() []*domain.ModelSpec {
	return []*domain.ModelSpec{
		{Alias: "cheap-img", JST: "cheap_img", Output: "image", EstCostHundredths: 100},
		{Alias: "cheap-vid", JST: "cheap_vid", Output: "video", EstCostHundredths: 500},
		{Alias: "pricey-img", JST: "pricey_img", Output: "image", EstCostHundredths: 5000},
		{Alias: "unlim-vid", JST: "unlim_vid", Output: "video", EstCostHundredths: 8000, RequiresUnlim: true},
	}
}

// fakeAccountStorePG is a minimal AccountStore that returns a preset
// slice from List so the /estimate unlim-override branch can be
// exercised. Every other method panics because /playground only reads
// via List.
type fakeAccountStorePG struct {
	unlimRows []domain.Account
}

func (f *fakeAccountStorePG) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	// The handler filters on HasUnlim=true + StatusActive; the tests only
	// exercise that specific call, so returning the preset slice is safe.
	return f.unlimRows, nil
}
func (f *fakeAccountStorePG) Get(context.Context, string) (*domain.Account, error) {
	panic("not implemented")
}
func (f *fakeAccountStorePG) Upsert(context.Context, *domain.Account) error {
	panic("not implemented")
}
func (f *fakeAccountStorePG) UpdateBalance(context.Context, string, int64, int64, int64) error {
	panic("not implemented")
}
func (f *fakeAccountStorePG) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	panic("not implemented")
}
func (f *fakeAccountStorePG) UpdateInFlight(context.Context, string, int) error {
	panic("not implemented")
}
func (f *fakeAccountStorePG) MarkStatus(context.Context, string, domain.AccountStatus, string) error {
	panic("not implemented")
}
func (f *fakeAccountStorePG) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	panic("not implemented")
}
func (f *fakeAccountStorePG) Unlock(context.Context, string, string) error {
	panic("not implemented")
}

// newPlaygroundHandler builds a Handler wired with the given key + spec
// list. The returned http.Handler mimics the server.go mount by
// preseeding the request context with the APIKey (skipping APIKeyAuth).
func newPlaygroundHandler(t *testing.T, key *domain.APIKey, accounts ports.AccountStore) (*Handler, http.Handler) {
	t.Helper()
	reg := &fakeModelRegistry{specs: playgroundSpecs()}
	h := &Handler{Registry: reg, Accounts: accounts}
	mux := http.NewServeMux()
	inject := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if key != nil {
				ctx := middleware.ContextWithAPIKey(r.Context(), key)
				r = r.WithContext(ctx)
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/v1/playground/models", inject(h.HandlePlaygroundModels))
	mux.HandleFunc("/v1/playground/estimate", inject(h.HandlePlaygroundEstimate))
	mux.HandleFunc("/v1/playground/execute", inject(h.HandlePlaygroundExecute))
	return h, mux
}

func decodePlaygroundBody(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func TestPlaygroundModels_CheapScope(t *testing.T) {
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeCheap}
	_, mux := newPlaygroundHandler(t, key, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/playground/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := decodePlaygroundBody(t, rec.Body)
	if got, _ := body["scope"].(string); got != "cheap" {
		t.Errorf("scope: got %q want cheap", got)
	}
	data, _ := body["data"].([]any)
	if len(data) != 4 {
		t.Fatalf("data len: got %d want 4", len(data))
	}
	// Expected allow map: est_cost <= 500 => allowed under cheap scope.
	want := map[string]bool{
		"cheap-img":  true,
		"cheap-vid":  true, // exactly at 500 => allowed
		"pricey-img": false,
		"unlim-vid":  false,
	}
	for _, item := range data {
		row := item.(map[string]any)
		id, _ := row["id"].(string)
		allowed, _ := row["allowed"].(bool)
		if allowed != want[id] {
			t.Errorf("id=%s allowed: got %v want %v", id, allowed, want[id])
		}
		reason, hasReason := row["blocked_reason"].(string)
		if !allowed {
			if !hasReason || reason != "cost_too_high" {
				t.Errorf("id=%s blocked_reason: got %q want cost_too_high", id, reason)
			}
		}
	}
}

func TestPlaygroundModels_FullScope(t *testing.T) {
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeFull}
	_, mux := newPlaygroundHandler(t, key, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/playground/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := decodePlaygroundBody(t, rec.Body)
	if got, _ := body["scope"].(string); got != "full" {
		t.Errorf("scope: got %q want full", got)
	}
	data, _ := body["data"].([]any)
	for _, item := range data {
		row := item.(map[string]any)
		id, _ := row["id"].(string)
		allowed, _ := row["allowed"].(bool)
		if !allowed {
			t.Errorf("id=%s: expected allowed under full scope", id)
		}
		if _, hasReason := row["blocked_reason"]; hasReason {
			t.Errorf("id=%s: full scope entries must not carry blocked_reason", id)
		}
	}
}

func TestPlaygroundModels_NoneScope(t *testing.T) {
	// Reachable only if a caller bypasses the middleware; the handler
	// must still fail closed so no row is flagged allowed.
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeNone}
	_, mux := newPlaygroundHandler(t, key, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/playground/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := decodePlaygroundBody(t, rec.Body)
	data, _ := body["data"].([]any)
	for _, item := range data {
		row := item.(map[string]any)
		allowed, _ := row["allowed"].(bool)
		if allowed {
			t.Errorf("row allowed under scope=none: %v", row)
		}
	}
}

func TestPlaygroundEstimate_AllowedCheap(t *testing.T) {
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeCheap}
	_, mux := newPlaygroundHandler(t, key, nil)

	body, _ := json.Marshal(map[string]any{"model": "cheap-img"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	got := decodePlaygroundBody(t, rec.Body)
	if alias, _ := got["model_alias"].(string); alias != "cheap-img" {
		t.Errorf("model_alias: got %q want cheap-img", alias)
	}
	if costH, _ := got["cost_credits_h"].(float64); costH != 100 {
		t.Errorf("cost_credits_h: got %v want 100", costH)
	}
	if wc, _ := got["will_charge"].(bool); !wc {
		t.Errorf("will_charge: got %v want true", wc)
	}
}

func TestPlaygroundEstimate_BlockedByScope(t *testing.T) {
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeCheap}
	_, mux := newPlaygroundHandler(t, key, nil)

	body, _ := json.Marshal(map[string]any{"model": "pricey-img"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	got := decodePlaygroundBody(t, rec.Body)
	errObj, _ := got["error"].(map[string]any)
	if k, _ := errObj["type"].(string); k != "blocked_by_scope" {
		t.Errorf("error type: got %q want blocked_by_scope", k)
	}
}

func TestPlaygroundEstimate_UnlimOverride(t *testing.T) {
	// A full-scope key resolving a RequiresUnlim model with at least one
	// unlim account live in the pool must report will_charge=false.
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeFull}
	accounts := &fakeAccountStorePG{unlimRows: []domain.Account{
		{ID: "a1", HasUnlim: true, Status: domain.StatusActive},
	}}
	_, mux := newPlaygroundHandler(t, key, accounts)

	body, _ := json.Marshal(map[string]any{"model": "unlim-vid"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	got := decodePlaygroundBody(t, rec.Body)
	if wc, _ := got["will_charge"].(bool); wc {
		t.Errorf("will_charge: got %v want false (unlim override active)", wc)
	}
}

func TestPlaygroundEstimate_UnlimNoAccounts(t *testing.T) {
	// A RequiresUnlim model without any pool-side unlim account still
	// charges the caller, so will_charge stays true.
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeFull}
	accounts := &fakeAccountStorePG{}
	_, mux := newPlaygroundHandler(t, key, accounts)

	body, _ := json.Marshal(map[string]any{"model": "unlim-vid"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	got := decodePlaygroundBody(t, rec.Body)
	if wc, _ := got["will_charge"].(bool); !wc {
		t.Errorf("will_charge: got %v want true (no unlim in pool)", wc)
	}
}

func TestPlaygroundEstimate_UnknownModel(t *testing.T) {
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeFull}
	_, mux := newPlaygroundHandler(t, key, nil)

	body, _ := json.Marshal(map[string]any{"model": "does-not-exist"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (body=%q)", rec.Code, rec.Body.String())
	}
}

// executeCaptureService is a proxy.Service stand-in for /execute: the test
// swaps the wrapped image/video handlers via reflection-free surgery by
// installing a fake proxy.Service that captures the resolved model in its
// Generate call. That way we don't need real upstream + accounts wiring.
//
// The Handler.Service field is *proxy.Service, so we construct one whose
// dependencies are nil except for the fields Generate touches on the
// bail-out path. Registry + Store are consulted, so we point Registry at
// the fake and Store at a fake that fails PickAndLock with a sentinel we
// then assert on.
func TestPlaygroundExecute_ForwardsToImageHandler(t *testing.T) {
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeFull}
	reg := &fakeModelRegistry{specs: playgroundSpecs()}
	// Build a proxy.Service whose Generate we can observe via the account
	// store's PickAndLock returning ErrNoEligibleAccount — a real path the
	// image handler forwards as 503 no_account_available. That confirms
	// execute reached HandleImageGeneration (not the video branch).
	accounts := &pickFailStore{err: domain.ErrNoEligibleAccount}
	svc := &proxy.Service{Store: accounts, Registry: reg}
	h := &Handler{Registry: reg, Service: svc, Accounts: accounts}
	mux := http.NewServeMux()
	inject := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(middleware.ContextWithAPIKey(r.Context(), key))
			next(w, r)
		}
	}
	mux.HandleFunc("/v1/playground/execute", inject(h.HandlePlaygroundExecute))

	body, _ := json.Marshal(map[string]any{"model": "cheap-img", "prompt": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/execute", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503 (body=%q)", rec.Code, rec.Body.String())
	}
	got := decodePlaygroundBody(t, rec.Body)
	errObj, _ := got["error"].(map[string]any)
	if k, _ := errObj["type"].(string); k != "no_account_available" {
		t.Errorf("error type: got %q want no_account_available", k)
	}
}

func TestPlaygroundExecute_BlockedByScope(t *testing.T) {
	key := &domain.APIKey{ID: "k", PlaygroundScope: domain.PlaygroundScopeCheap}
	_, mux := newPlaygroundHandler(t, key, nil)

	body, _ := json.Marshal(map[string]any{"model": "pricey-img", "prompt": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/execute", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (body=%q)", rec.Code, rec.Body.String())
	}
}

// pickFailStore is a minimal AccountStore that fails PickAndLock so
// /execute forwards a mapped generation error. Every other method panics.
type pickFailStore struct {
	err error
}

func (s *pickFailStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	// /execute also invokes hasUnlimAccountAvailable via /estimate flow
	// only. On this path (execute) we do not call List, but the interface
	// still needs a stub.
	return nil, nil
}
func (s *pickFailStore) Get(context.Context, string) (*domain.Account, error) {
	panic("not implemented")
}
func (s *pickFailStore) Upsert(context.Context, *domain.Account) error {
	panic("not implemented")
}
func (s *pickFailStore) UpdateBalance(context.Context, string, int64, int64, int64) error {
	panic("not implemented")
}
func (s *pickFailStore) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	panic("not implemented")
}
func (s *pickFailStore) UpdateInFlight(context.Context, string, int) error {
	panic("not implemented")
}
func (s *pickFailStore) MarkStatus(context.Context, string, domain.AccountStatus, string) error {
	panic("not implemented")
}
func (s *pickFailStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	return nil, "", s.err
}
func (s *pickFailStore) Unlock(context.Context, string, string) error {
	panic("not implemented")
}
