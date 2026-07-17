// Package e2e_test stitches the admin surface, the /v1 public surface,
// the reverse-proxy service, and a hermetic upstream/clerk mock into a
// single happy-path integration test. Every collaborator lives in the
// same process — no real TCP listener is opened; requests are dispatched
// via chi.Router.ServeHTTP against an httptest.ResponseRecorder.
//
// The test asserts that a freshly minted API key can drive a synchronous
// image generation end-to-end (create → status → fetch → metering → job
// list → admin listing → purge), covering the same wiring the production
// binary uses without depending on any external service.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/api/admin"
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	v1 "github.com/greensheep999/higgsgo/internal/api/v1"
	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/core/metering"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// testAdminBearer is the shared secret the admin router expects on every
// /admin/* request. It never leaves this test file.
const testAdminBearer = "test-admin-bearer"

// e2eHarness bundles the collaborators the test flow drives. Kept small
// so the test body reads top-down rather than reaching into a fixture bag.
type e2eHarness struct {
	// HTTP routers.
	publicRouter chi.Router
	adminRouter  chi.Router

	// Stores backed by the in-memory sqlite DB.
	db       *sqlite.DB
	accounts ports.AccountStore
	jobs     ports.JobStore
	keys     ports.APIKeyStore

	// Mock upstream — one httptest.Server that answers fnf.higgsfield.ai
	// paths (create/status/fetch/wallet/user). Clerk mint calls are
	// short-circuited inside the fake HTTP client below.
	upstreamSrv *httptest.Server

	// Instrumented account inserted directly into the pool so PickAndLock
	// has a candidate. Its id is stable so the test can assert on it in
	// the admin job listing.
	accountID string
}

// TestE2E_HappyPath drives the full admin-create → v1-generate → job-list
// → purge flow. All ten checkpoints are asserted so any regression in
// the wiring is caught by a single failure.
func TestE2E_HappyPath(t *testing.T) {
	h := newHarness(t)

	// Checkpoint 1: admin create key returns 201 + plaintext key.
	plaintext := createAPIKey(t, h)
	if !strings.HasPrefix(plaintext, "sk-hg-") {
		t.Fatalf("plaintext key: got %q, expected sk-hg- prefix", plaintext)
	}

	// Checkpoint 2: unauth POST /v1/images/generations gets 401.
	// Runs before the happy path so the auth middleware surface is
	// exercised without polluting the pool with an in-flight job.
	{
		rr := doRequest(t, h.publicRouter, http.MethodPost, "/v1/images/generations", "",
			`{"model":"e2e-image","prompt":"hello"}`)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("unauth generate: got %d want 401 (body=%s)", rr.Code, rr.Body.String())
		}
	}

	// Checkpoint 3: POST /v1/images/generations with Bearer key returns
	// a completed sync response — the mock upstream is scripted so the
	// PollUntilTerminal loop finds terminal on the first status call.
	genBody := doGenerate(t, h, plaintext, http.StatusOK)
	if got := genBody["id"]; got != "job_e2e" {
		t.Fatalf("generate id: got %v want job_e2e (body=%v)", got, genBody)
	}
	if got := genBody["status"]; got != "completed" {
		t.Fatalf("generate status: got %v want completed (body=%v)", got, genBody)
	}
	if got := genBody["result_url"]; got != "https://x.com/y.png" {
		t.Fatalf("generate result_url: got %v (body=%v)", got, genBody)
	}

	// Checkpoint 4: GET /v1/jobs/{id} (public, scoped by api key)
	// resolves to the freshly created row.
	{
		rr := doRequest(t, h.publicRouter, http.MethodGet, "/v1/jobs/job_e2e", plaintext, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("public job fetch: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		body := parseJSON(t, rr.Body.Bytes())
		if body["id"] != "job_e2e" {
			t.Fatalf("public job fetch id: got %v want job_e2e", body["id"])
		}
	}

	// Checkpoint 5: GET /v1/jobs (public list, scoped) returns the row.
	{
		rr := doRequest(t, h.publicRouter, http.MethodGet, "/v1/jobs", plaintext, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("public jobs list: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		body := parseJSON(t, rr.Body.Bytes())
		rows, _ := body["data"].([]any)
		if len(rows) != 1 {
			t.Fatalf("public jobs list: got %d rows want 1 (body=%v)", len(rows), body)
		}
	}

	// Checkpoint 6: GET /admin/jobs returns the same row with account_id
	// echoed back (public view hides it, admin exposes it).
	{
		rr := doRequest(t, h.adminRouter, http.MethodGet, "/admin/jobs", testAdminBearer, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("admin jobs list: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		body := parseJSON(t, rr.Body.Bytes())
		rows, _ := body["data"].([]any)
		if len(rows) != 1 {
			t.Fatalf("admin jobs list: got %d rows want 1", len(rows))
		}
		first, _ := rows[0].(map[string]any)
		if first["account_id"] != h.accountID {
			t.Fatalf("admin job account_id: got %v want %s", first["account_id"], h.accountID)
		}
	}

	// Checkpoint 7: GET /admin/usage returns >= 1 usage_events row (the
	// sync path fires Recorder.OnJobTerminal so this must be populated).
	{
		rr := doRequest(t, h.adminRouter, http.MethodGet, "/admin/usage", testAdminBearer, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("admin usage: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		body := parseJSON(t, rr.Body.Bytes())
		rows, _ := body["data"].([]any)
		if len(rows) < 1 {
			t.Fatalf("admin usage: got %d rows want >= 1", len(rows))
		}
	}

	// Checkpoint 8: POST /admin/jobs/purge with future older_than removes
	// the row. usage_events is deliberately left in place so post-purge
	// billing history still resolves.
	{
		future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
		payload := fmt.Sprintf(`{"older_than":%q}`, future)
		rr := doRequest(t, h.adminRouter, http.MethodPost, "/admin/jobs/purge", testAdminBearer, payload)
		if rr.Code != http.StatusOK {
			t.Fatalf("admin purge: got %d want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		body := parseJSON(t, rr.Body.Bytes())
		if got, _ := body["purged"].(float64); got < 1 {
			t.Fatalf("admin purge: got purged=%v want >= 1", body["purged"])
		}
	}

	// Checkpoint 9: GET /v1/jobs after purge returns an empty list.
	{
		rr := doRequest(t, h.publicRouter, http.MethodGet, "/v1/jobs", plaintext, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("post-purge jobs list: got %d want 200", rr.Code)
		}
		body := parseJSON(t, rr.Body.Bytes())
		rows, _ := body["data"].([]any)
		if len(rows) != 0 {
			t.Fatalf("post-purge jobs list: got %d rows want 0", len(rows))
		}
	}

	// Checkpoint 10: GET /v1/jobs/job_e2e after purge returns 404.
	{
		rr := doRequest(t, h.publicRouter, http.MethodGet, "/v1/jobs/job_e2e", plaintext, "")
		if rr.Code != http.StatusNotFound {
			t.Fatalf("post-purge job fetch: got %d want 404", rr.Code)
		}
	}
}

// TestE2E_PausedKey confirms the auth middleware surfaces api_key_paused
// on a paused key rather than letting the request through. Runs against a
// fresh harness so the paused-key state does not leak into other tests.
func TestE2E_PausedKey(t *testing.T) {
	h := newHarness(t)
	plaintext := createAPIKey(t, h)

	// Extract the key id from the store (List returns every row).
	rr := doRequest(t, h.adminRouter, http.MethodGet, "/admin/keys", testAdminBearer, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list keys: got %d", rr.Code)
	}
	body := parseJSON(t, rr.Body.Bytes())
	rows, _ := body["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("keys list: got %d rows want 1", len(rows))
	}
	first, _ := rows[0].(map[string]any)
	keyID, _ := first["id"].(string)

	// Pause it.
	pauseRR := doRequest(t, h.adminRouter, http.MethodPost, "/admin/keys/"+keyID+"/pause", testAdminBearer, "")
	if pauseRR.Code != http.StatusOK {
		t.Fatalf("pause: got %d (body=%s)", pauseRR.Code, pauseRR.Body.String())
	}

	// Generate with the paused key returns 401 api_key_paused.
	genRR := doRequest(t, h.publicRouter, http.MethodPost, "/v1/images/generations", plaintext,
		`{"model":"e2e-image","prompt":"hello"}`)
	if genRR.Code != http.StatusUnauthorized {
		t.Fatalf("paused key generate: got %d want 401 (body=%s)", genRR.Code, genRR.Body.String())
	}
	errBody := parseJSON(t, genRR.Body.Bytes())
	errMap, _ := errBody["error"].(map[string]any)
	if got, _ := errMap["type"].(string); got != "api_key_paused" {
		t.Fatalf("paused key error type: got %q want api_key_paused", got)
	}
}

// -- harness setup -----------------------------------------------------

// newHarness builds the full in-process stack. The returned harness is
// cleaned up automatically via t.Cleanup.
func newHarness(t *testing.T) *e2eHarness {
	t.Helper()
	ctx := context.Background()

	db, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	accStore := sqlite.NewAccountStore(db)
	jobStore := sqlite.NewJobStore(db)
	keyStore := sqlite.NewAPIKeyStore(db)
	usageStore := sqlite.NewUsageEventStore(db)

	// Insert a fake active account. Cookies and session_id are non-empty
	// so the JWT minter's guard clauses pass; the real mint request
	// itself is short-circuited by fakeUpstreamHTTP.
	accountID := "acc_e2e"
	err = accStore.Upsert(ctx, &domain.Account{
		ID:                  accountID,
		Email:               "e2e@example.com",
		Password:            "unused",
		SessionID:           "sess_e2e",
		CookiesJSON:         `{"__session":"cookie","datadome":"dd"}`,
		UserAgent:           "e2e-UA",
		DataDomeClientID:    "dd-cid",
		WorkspaceID:         "ws_e2e",
		PlanType:            domain.PlanPlus,
		SubscriptionBalance: 100000, // 1000 credits — plenty
		Status:              domain.StatusActive,
		RegisteredAt:        time.Now().UTC(),
		ImportedAt:          time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("upsert e2e account: %v", err)
	}

	// Mock upstream (fnf.higgsfield.ai). The handler dispatches by URL
	// path — see mockUpstreamHandler below.
	upstreamSrv := httptest.NewServer(mockUpstreamHandler())
	t.Cleanup(upstreamSrv.Close)

	// Fake ports.UpstreamClient. Clerk mint requests are answered inline
	// with a canned JWT so the JWT minter never leaves the process.
	// Everything else is dispatched to Go's default transport, which
	// hits the upstreamSrv URL because upstream.Client is built with
	// BaseURL pointing at that server.
	fakeHTTP := &fakeUpstreamHTTP{t: t}
	minter := jwt.New(fakeHTTP, ports.RealClock{}, jwt.Config{})
	upClient := upstream.New(fakeHTTP, minter, upstream.Config{BaseURL: upstreamSrv.URL})

	// Static registry with a single image model whose JST + endpoint
	// match what the mock upstream expects.
	reg := &staticRegistry{specs: []*domain.ModelSpec{{
		Alias:             "e2e-image",
		JST:               "e2e_image",
		Endpoint:          "/jobs/v2/e2e_image",
		Version:           "v2",
		Output:            "image",
		EstCostHundredths: 500,
	}}}

	// Metering recorder writes into the same in-memory usage_events table.
	rec := &metering.Recorder{
		Events:   usageStore,
		APIKeys:  keyStore,
		Accounts: accStore,
	}

	svc := &proxy.Service{
		Store:            accStore,
		Registry:         reg,
		Upstream:         upClient,
		Jobs:             jobStore,
		Clock:            ports.RealClock{},
		APIKeys:          keyStore,
		Meter:            rec,
		AsyncByDefault:   false, // force sync path so poll wraps within the request
		SyncPollDeadline: 5 * time.Second,
	}

	// Public router: APIKeyAuth on /v1/*.
	publicRouter := chi.NewRouter()
	handler := v1.New(svc, reg, jobStore, nil, keyStore)
	publicRouter.Route("/v1", func(r chi.Router) {
		r.Use(middleware.APIKeyAuth(keyStore, false))
		r.Post("/images/generations", handler.HandleImageGeneration)
		r.Get("/jobs", handler.HandleJobsList)
		r.Get("/jobs/{id}", handler.HandleJobFetch)
	})

	// Admin router: BearerAuth(testAdminBearer) on /admin/*.
	adminRouter := chi.NewRouter()
	keysH := admin.NewKeysHandler(keyStore)
	jobsH := admin.NewJobsHandler(jobStore)
	usageH := admin.NewUsageHandler(usageStore)
	adminRouter.Route("/admin", func(r chi.Router) {
		r.Use(middleware.BearerAuth(testAdminBearer))
		keysH.Register(r)
		jobsH.Register(r)
		usageH.Register(r)
	})

	return &e2eHarness{
		publicRouter: publicRouter,
		adminRouter:  adminRouter,
		db:           db,
		accounts:     accStore,
		jobs:         jobStore,
		keys:         keyStore,
		upstreamSrv:  upstreamSrv,
		accountID:    accountID,
	}
}

// -- mock upstream -----------------------------------------------------

// mockUpstreamHandler answers the four fnf.higgsfield.ai paths the proxy
// service touches on a happy-path sync image generation, plus the two
// balance-refresher endpoints (wallet, user) that a background ticker
// might drive if enabled — kept here for completeness so extending the
// harness later does not require wiring another server.
func mockUpstreamHandler() http.Handler {
	// Track requests so tests can assert on hit counts if desired.
	// Currently unused, but keeps the door open for follow-up asserts
	// without needing to modify the handler.
	var hits atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/jobs/v2/e2e_image":
			// Mirror the shape parseCreateResponse expects: nested job_sets.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": "js_e2e",
				"job_sets": [{
					"id": "js_e2e",
					"cost": 500,
					"jobs": [{"id": "job_e2e"}]
				}]
			}`))
		case r.Method == http.MethodGet && r.URL.Path == "/jobs/job_e2e/status":
			// Terminal on the first poll so the sync loop returns fast.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"job_e2e","status":"completed","job_set_type":"e2e_image"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/jobs/job_e2e":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": "job_e2e",
				"status": "completed",
				"meta": {"is_refunded": false},
				"results": {"raw": {"url": "https://x.com/y.png"}}
			}`))
		case r.Method == http.MethodGet && r.URL.Path == "/workspaces/wallet":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"workspace_id":"ws_e2e","subscription_balance":99500,"credits_balance":0}`))
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"user_e2e","email":"e2e@example.com","plan_type":"plus"}`))
		default:
			http.Error(w, "e2e upstream: unexpected "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
		}
	})
}

// fakeUpstreamHTTP is a ports.UpstreamClient shim. Clerk mint requests
// are answered inline with a canned JWT so the JWT minter never talks to
// clerk.higgsfield.ai — everything else falls through to the default
// transport, which reaches the mock upstream because upstream.Client was
// built with BaseURL = upstreamSrv.URL.
type fakeUpstreamHTTP struct {
	t *testing.T
}

func (f *fakeUpstreamHTTP) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		token := newFakeJWT(f.t, "user_e2e")
		body := fmt.Sprintf(`{"jwt":%q}`, token)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return http.DefaultClient.Do(req.WithContext(ctx))
}

func (f *fakeUpstreamHTTP) Fingerprint() string { return "e2e-fake" }
func (f *fakeUpstreamHTTP) Name() string        { return "e2e-fake" }

// newFakeJWT builds a valid 3-segment JWT whose exp claim is 1h out so
// the minter's slack-window check keeps the token cached across the
// couple of retries the sync flow triggers.
func newFakeJWT(t *testing.T, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := map[string]any{
		"sub":   sub,
		"email": "e2e@example.com",
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}
	claimBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal fake jwt claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + payload + "." + sig
}

// -- static model registry --------------------------------------------

// staticRegistry is a ports.ModelRegistry backed by an in-memory slice.
// Kept private to this file so the test remains self-contained per the
// task constraints (no shared fake registry across packages).
type staticRegistry struct {
	specs []*domain.ModelSpec
}

func (r *staticRegistry) Resolve(alias string) (*domain.ModelSpec, error) {
	for _, s := range r.specs {
		if s.Alias == alias {
			return s, nil
		}
	}
	return nil, domain.ErrModelNotFound
}

func (r *staticRegistry) List(filter ports.ModelFilter) []*domain.ModelSpec {
	out := make([]*domain.ModelSpec, 0, len(r.specs))
	for _, s := range r.specs {
		if filter.Output != "" && s.Output != filter.Output {
			continue
		}
		if !filter.IncludeUnstable && s.Unstable {
			continue
		}
		if !filter.IncludeDeprecated && s.Deprecated {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (r *staticRegistry) Reload(ctx context.Context) error     { return nil }
func (r *staticRegistry) ResolveAlias(a string) (string, bool) { return a, true }
func (r *staticRegistry) StarterLocked(jst string) bool        { return false }

// -- helpers -----------------------------------------------------------

// doRequest builds a request, drives it through the router via
// ServeHTTP, and returns the recorder. authToken is either the admin
// bearer (for /admin routes) or the API key plaintext (for /v1 routes).
// An empty authToken suppresses the Authorization header entirely.
func doRequest(t *testing.T, r chi.Router, method, path, authToken, body string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// createAPIKey POSTs to /admin/keys and returns the plaintext key.
func createAPIKey(t *testing.T, h *e2eHarness) string {
	t.Helper()
	rr := doRequest(t, h.adminRouter, http.MethodPost, "/admin/keys", testAdminBearer,
		`{"name":"e2e-key"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create key: got %d want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	body := parseJSON(t, rr.Body.Bytes())
	pt, _ := body["plaintext_key"].(string)
	if pt == "" {
		t.Fatalf("create key: missing plaintext_key (body=%v)", body)
	}
	return pt
}

// doGenerate POSTs to /v1/images/generations and asserts the HTTP code.
// The parsed response is returned so the caller can drill into fields.
func doGenerate(t *testing.T, h *e2eHarness, apiKey string, wantStatus int) map[string]any {
	t.Helper()
	rr := doRequest(t, h.publicRouter, http.MethodPost, "/v1/images/generations", apiKey,
		`{"model":"e2e-image","prompt":"a red apple"}`)
	if rr.Code != wantStatus {
		t.Fatalf("generate: got %d want %d (body=%s)", rr.Code, wantStatus, rr.Body.String())
	}
	return parseJSON(t, rr.Body.Bytes())
}

// parseJSON decodes a JSON object into a generic map; test failures on
// malformed responses are fatal so the caller does not need to guard.
func parseJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode json: %v (raw=%s)", err, raw)
	}
	return out
}
