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
	"time"

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
func (f *fakeAccountStorePG) MarkThrottled(context.Context, string, time.Time, string) error {
	panic("not implemented")
}
func (f *fakeAccountStorePG) RecoverThrottled(context.Context) (int, error) { return 0, nil }
func (f *fakeAccountStorePG) IncrFailStreak(context.Context, string) (int, error) {
	panic("not implemented")
}
func (f *fakeAccountStorePG) ResetFailStreak(context.Context, string) error {
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
func (s *pickFailStore) MarkThrottled(context.Context, string, time.Time, string) error {
	panic("not implemented")
}
func (s *pickFailStore) RecoverThrottled(context.Context) (int, error) { return 0, nil }
func (s *pickFailStore) IncrFailStreak(context.Context, string) (int, error) {
	panic("not implemented")
}
func (s *pickFailStore) ResetFailStreak(context.Context, string) error {
	panic("not implemented")
}
func (s *pickFailStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	return nil, "", s.err
}
func (s *pickFailStore) Unlock(context.Context, string, string) error {
	panic("not implemented")
}

// fakeAPIKeyStorePG is a minimal APIKeyStore for the as_api_key_id
// impersonation tests. Only Get is exercised; every other method is
// inherited from the embedded (nil) interface and panics if called, which
// keeps the fake honest about what the handler actually touches.
type fakeAPIKeyStorePG struct {
	ports.APIKeyStore
	byID map[string]*domain.APIKey
	err  error
}

func (f *fakeAPIKeyStorePG) Get(_ context.Context, id string) (*domain.APIKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	k, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrAPIKeyNotFound
	}
	return k, nil
}

// newAdminPlaygroundHandler wires a Handler as if the caller authenticated
// with the admin bearer (IsAdminBearer=true) rather than an sk-hg- key. The
// optional apiKeys store backs as_api_key_id resolution.
func newAdminPlaygroundHandler(t *testing.T, apiKeys ports.APIKeyStore, accounts ports.AccountStore) (*Handler, http.Handler) {
	t.Helper()
	reg := &fakeModelRegistry{specs: playgroundSpecs()}
	h := &Handler{Registry: reg, Accounts: accounts, APIKeys: apiKeys}
	mux := http.NewServeMux()
	inject := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(middleware.WithAdminBearer(r.Context()))
			next(w, r)
		}
	}
	mux.HandleFunc("/v1/playground/estimate", inject(h.HandlePlaygroundEstimate))
	mux.HandleFunc("/v1/playground/execute", inject(h.HandlePlaygroundExecute))
	return h, mux
}

func TestPlaygroundEstimate_AdminNoAsKey_UsesFullScope(t *testing.T) {
	// Admin bearer without as_api_key_id keeps the historical behaviour:
	// full scope, so a pricey model still estimates fine.
	_, mux := newAdminPlaygroundHandler(t, nil, nil)

	body, _ := json.Marshal(map[string]any{"model": "pricey-img"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestPlaygroundEstimate_AdminAsKeyCheap_GatesByKeyScope(t *testing.T) {
	// Admin impersonating a cheap-scope key must be gated by that key's
	// scope: a pricey model is rejected with blocked_by_scope even though
	// the admin bearer itself would be full.
	keys := &fakeAPIKeyStorePG{byID: map[string]*domain.APIKey{
		"k-cheap": {ID: "k-cheap", Status: domain.APIKeyStatusActive, PlaygroundScope: domain.PlaygroundScopeCheap},
	}}
	_, mux := newAdminPlaygroundHandler(t, keys, nil)

	body, _ := json.Marshal(map[string]any{"model": "pricey-img", "as_api_key_id": "k-cheap"})
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

	// The same key permits a cheap model.
	body2, _ := json.Marshal(map[string]any{"model": "cheap-img", "as_api_key_id": "k-cheap"})
	req2 := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body2))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("cheap model status: got %d want 200 (body=%q)", rec2.Code, rec2.Body.String())
	}
}

func TestPlaygroundEstimate_AdminAsKeyUnknown_Rejected(t *testing.T) {
	keys := &fakeAPIKeyStorePG{byID: map[string]*domain.APIKey{}}
	_, mux := newAdminPlaygroundHandler(t, keys, nil)

	body, _ := json.Marshal(map[string]any{"model": "cheap-img", "as_api_key_id": "nope"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	got := decodePlaygroundBody(t, rec.Body)
	errObj, _ := got["error"].(map[string]any)
	if k, _ := errObj["type"].(string); k != "invalid_as_api_key" {
		t.Errorf("error type: got %q want invalid_as_api_key", k)
	}
}

func TestPlaygroundEstimate_AdminAsKeyNoneScope_Disabled(t *testing.T) {
	keys := &fakeAPIKeyStorePG{byID: map[string]*domain.APIKey{
		"k-none": {ID: "k-none", Status: domain.APIKeyStatusActive, PlaygroundScope: domain.PlaygroundScopeNone},
	}}
	_, mux := newAdminPlaygroundHandler(t, keys, nil)

	body, _ := json.Marshal(map[string]any{"model": "cheap-img", "as_api_key_id": "k-none"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (body=%q)", rec.Code, rec.Body.String())
	}
	got := decodePlaygroundBody(t, rec.Body)
	errObj, _ := got["error"].(map[string]any)
	if k, _ := errObj["type"].(string); k != "playground_disabled" {
		t.Errorf("error type: got %q want playground_disabled", k)
	}
}

func TestPlaygroundEstimate_AdminAsKeyRevoked_Rejected(t *testing.T) {
	keys := &fakeAPIKeyStorePG{byID: map[string]*domain.APIKey{
		"k-rev": {ID: "k-rev", Status: domain.APIKeyStatusRevoked, PlaygroundScope: domain.PlaygroundScopeFull},
	}}
	_, mux := newAdminPlaygroundHandler(t, keys, nil)

	body, _ := json.Marshal(map[string]any{"model": "cheap-img", "as_api_key_id": "k-rev"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/estimate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	got := decodePlaygroundBody(t, rec.Body)
	errObj, _ := got["error"].(map[string]any)
	if k, _ := errObj["type"].(string); k != "invalid_as_api_key" {
		t.Errorf("error type: got %q want invalid_as_api_key", k)
	}
}

func TestPlaygroundExecute_AdminAsKey_ForwardsUnderKeyScope(t *testing.T) {
	// An admin bearer impersonating a full-scope key forwards to the image
	// handler; the no-eligible-account path maps to 503, confirming the
	// per-model gate passed under the impersonated scope and the request
	// reached HandleImageGeneration.
	asKey := &domain.APIKey{ID: "k-full", Status: domain.APIKeyStatusActive, PlaygroundScope: domain.PlaygroundScopeFull}
	keys := &fakeAPIKeyStorePG{byID: map[string]*domain.APIKey{"k-full": asKey}}
	reg := &fakeModelRegistry{specs: playgroundSpecs()}
	accounts := &pickFailStore{err: domain.ErrNoEligibleAccount}
	svc := &proxy.Service{Store: accounts, Registry: reg}
	h := &Handler{Registry: reg, Service: svc, Accounts: accounts, APIKeys: keys}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/playground/execute", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(middleware.WithAdminBearer(r.Context()))
		h.HandlePlaygroundExecute(w, r)
	})

	body, _ := json.Marshal(map[string]any{"model": "cheap-img", "prompt": "hi", "as_api_key_id": "k-full"})
	req := httptest.NewRequest(http.MethodPost, "/v1/playground/execute", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503 (body=%q)", rec.Code, rec.Body.String())
	}

	// Direct unit assertion on the identity resolver: admin + as_api_key_id
	// returns the impersonated key so execute injects it into the context
	// (which drives downstream usage / markup / group routing / quota).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/playground/execute", nil)
	req2 = req2.WithContext(middleware.WithAdminBearer(req2.Context()))
	scope, k, herr := h.resolveExecutionIdentity(req2, "k-full")
	if herr != nil {
		t.Fatalf("resolveExecutionIdentity: unexpected error %+v", herr)
	}
	if scope != domain.PlaygroundScopeFull {
		t.Errorf("scope: got %q want full", scope)
	}
	if k == nil || k.ID != "k-full" {
		t.Errorf("resolved key: got %+v want k-full", k)
	}
}

func TestResolveExecutionIdentity_SkKeyIgnoresAsKeyID(t *testing.T) {
	// A real sk-hg- caller (key in context, not admin bearer) must run as
	// itself and ignore as_api_key_id entirely — no impersonation, nil key.
	selfKey := &domain.APIKey{ID: "self", Status: domain.APIKeyStatusActive, PlaygroundScope: domain.PlaygroundScopeCheap}
	other := &domain.APIKey{ID: "other", Status: domain.APIKeyStatusActive, PlaygroundScope: domain.PlaygroundScopeFull}
	keys := &fakeAPIKeyStorePG{byID: map[string]*domain.APIKey{"other": other}}
	h := &Handler{APIKeys: keys}

	req := httptest.NewRequest(http.MethodPost, "/v1/playground/execute", nil)
	req = req.WithContext(middleware.ContextWithAPIKey(req.Context(), selfKey))
	scope, k, herr := h.resolveExecutionIdentity(req, "other")
	if herr != nil {
		t.Fatalf("resolveExecutionIdentity: unexpected error %+v", herr)
	}
	if scope != domain.PlaygroundScopeCheap {
		t.Errorf("scope: got %q want cheap (self scope, not impersonated)", scope)
	}
	if k != nil {
		t.Errorf("resolved key: got %+v want nil (sk-hg- caller runs as self)", k)
	}
}
