package refresher

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// balanceCall captures one UpdateBalance invocation for assertion.
type balanceCall struct {
	ID  string
	Sub int64
	Cr  int64
	Pkg int64
}

// fakeAccountStore is a minimal ports.AccountStore for refresher tests.
// Only List / UpdateBalance / UpdateEntitlements do real work; every other
// method panics so an accidental dependency shows up immediately.
//
// The three sync fields (quotaCalls / unlimReplaces / listUnlimResp)
// were added when the refresher gained free-quota + unlim-activation
// syncing (task 6 of the load-balance dormant flag wiring). They record
// what the ticker persisted per account so tests can assert on the
// captured side effects without hitting a real DB.
type fakeAccountStore struct {
	mu       sync.Mutex
	accounts []domain.Account
	balances []balanceCall
	entitles []struct {
		ID  string
		Ent ports.EntitlementUpdate
	}
	quotaCalls    []quotaCall
	unlimReplaces []unlimReplaceCall
}

// quotaCall captures one UpdateFreeQuota invocation.
type quotaCall struct {
	ID string
	Q  domain.FreeQuotaCounters
}

// unlimReplaceCall captures one ReplaceUnlimActivations invocation.
type unlimReplaceCall struct {
	ID          string
	Activations []domain.UnlimActivation
}

func (f *fakeAccountStore) List(ctx context.Context, filter ports.AccountFilter) ([]domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Account, len(f.accounts))
	copy(out, f.accounts)
	return out, nil
}

func (f *fakeAccountStore) UpdateBalance(ctx context.Context, id string, sub, credits, pkg int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances = append(f.balances, balanceCall{ID: id, Sub: sub, Cr: credits, Pkg: pkg})
	return nil
}

func (f *fakeAccountStore) UpdateEntitlements(ctx context.Context, id string, e ports.EntitlementUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entitles = append(f.entitles, struct {
		ID  string
		Ent ports.EntitlementUpdate
	}{ID: id, Ent: e})
	return nil
}

// The rest are never called by refresher; panic to catch regressions.
func (f *fakeAccountStore) Get(ctx context.Context, id string) (*domain.Account, error) {
	panic("fakeAccountStore.Get not implemented")
}
func (f *fakeAccountStore) Upsert(ctx context.Context, a *domain.Account) error {
	panic("fakeAccountStore.Upsert not implemented")
}
func (f *fakeAccountStore) UpdateInFlight(ctx context.Context, id string, delta int) error {
	panic("fakeAccountStore.UpdateInFlight not implemented")
}
func (f *fakeAccountStore) ResetAllInFlight(ctx context.Context) (int, error) { return 0, nil }
func (f *fakeAccountStore) MarkStatus(ctx context.Context, id string, s domain.AccountStatus, reason string) error {
	panic("fakeAccountStore.MarkStatus not implemented")
}
func (f *fakeAccountStore) MarkThrottled(ctx context.Context, id string, until time.Time, reason string) error {
	panic("fakeAccountStore.MarkThrottled not implemented")
}
func (f *fakeAccountStore) RecoverThrottled(ctx context.Context) (int, error) {
	return 0, nil
}
func (f *fakeAccountStore) IncrFailStreak(ctx context.Context, id string) (int, error) {
	panic("fakeAccountStore.IncrFailStreak not implemented")
}
func (f *fakeAccountStore) ResetFailStreak(ctx context.Context, id string) error {
	panic("fakeAccountStore.ResetFailStreak not implemented")
}
func (f *fakeAccountStore) PickAndLock(ctx context.Context, p ports.PickParams) (*domain.Account, string, error) {
	panic("fakeAccountStore.PickAndLock not implemented")
}
func (f *fakeAccountStore) Unlock(ctx context.Context, id, tok string) error {
	panic("fakeAccountStore.Unlock not implemented")
}
func (f *fakeAccountStore) UpdateFreeQuota(_ context.Context, id string, q domain.FreeQuotaCounters) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotaCalls = append(f.quotaCalls, quotaCall{ID: id, Q: q})
	return nil
}
func (f *fakeAccountStore) ListUnlimActivations(_ context.Context, _ string) ([]domain.UnlimActivation, error) {
	return nil, nil
}
func (f *fakeAccountStore) ReplaceUnlimActivations(_ context.Context, id string, a []domain.UnlimActivation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unlimReplaces = append(f.unlimReplaces, unlimReplaceCall{ID: id, Activations: append([]domain.UnlimActivation(nil), a...)})
	return nil
}

// fakeHTTPClient short-circuits clerk.higgsfield.ai to a stub JWT-mint
// response and forwards everything else to the real HTTP transport so the
// httptest.Server we spin up per test can serve /workspaces/wallet + /user.
type fakeHTTPClient struct {
	mintJWT string
}

func (f *fakeHTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		body := fmt.Sprintf(`{"jwt":%q}`, f.mintJWT)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return http.DefaultClient.Do(req.WithContext(ctx))
}

func (f *fakeHTTPClient) Fingerprint() string { return "fake" }
func (f *fakeHTTPClient) Name() string        { return "fake" }

func newFakeJWT(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := map[string]any{
		"sub":   "user_test",
		"email": "test@example.com",
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}
	claimBytes, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(claimBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + payload + "." + sig
}

// mkAccount builds a minimal active account whose cookies_json carries the
// given marker so the test server can distinguish requests per account.
func mkAccount(id, marker string) domain.Account {
	return domain.Account{
		ID:          id,
		Email:       id + "@example.com",
		SessionID:   "sess_" + id,
		CookiesJSON: fmt.Sprintf(`{"__session":"stub","x-marker":%q}`, marker),
		UserAgent:   "Mozilla/5.0 (test)",
		Status:      domain.StatusActive,
	}
}

// discardLogger keeps test output clean.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newRefresher builds a refresher pointing at the given httptest.Server.
func newRefresher(t *testing.T, srv *httptest.Server, store ports.AccountStore, concurrency int) *Refresher {
	t.Helper()
	fake := &fakeHTTPClient{mintJWT: newFakeJWT(t)}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	up := upstream.New(fake, minter, upstream.Config{BaseURL: srv.URL})
	return &Refresher{
		Accounts:    store,
		Upstream:    up,
		Logger:      discardLogger(),
		Interval:    time.Hour, // never fires; tests drive tick() directly
		Concurrency: concurrency,
	}
}

// walletJSON / userJSON keep fixture bodies short and reusable.
const (
	walletJSON = `{"workspace_id":"ws_1","subscription_balance":88000,"credits_balance":1200,"total_credits":100000,"on_demand_credits":0}`
	userJSON   = `{"id":"user_x","email":"e","plan_type":"plus","has_unlim":true,"has_flex_unlim":false,"is_pro_plan_veo3_available":true,"cohort":"c1","total_plan_credits":100.0,"plan_ends_at":"2026-08-17T10:00:00Z","workspace_id":"ws_1"}`
)

func TestRefresher_TickUpdatesBothBalanceAndEntitlements(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userJSON))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeAccountStore{
		accounts: []domain.Account{
			mkAccount("acc_1", "m1"),
			mkAccount("acc_2", "m2"),
		},
	}
	r := newRefresher(t, srv, store, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.tick(ctx)

	if len(store.balances) != 2 {
		t.Fatalf("expected 2 UpdateBalance calls, got %d", len(store.balances))
	}
	if len(store.entitles) != 2 {
		t.Fatalf("expected 2 UpdateEntitlements calls, got %d", len(store.entitles))
	}

	// Spot-check that wallet fields arrived intact.
	for _, b := range store.balances {
		if b.Sub != 88000 || b.Cr != 1200 || b.Pkg != 100000 {
			t.Errorf("balance %s: got sub=%d cr=%d pkg=%d", b.ID, b.Sub, b.Cr, b.Pkg)
		}
	}
	// And entitlement fields. total_plan_credits = 100.0 → 10000 hundredths.
	for _, e := range store.entitles {
		if e.Ent.PlanType != domain.PlanPlus {
			t.Errorf("entitle %s: plan %s want plus", e.ID, e.Ent.PlanType)
		}
		if !e.Ent.HasUnlim {
			t.Errorf("entitle %s: HasUnlim false", e.ID)
		}
		if e.Ent.HasFlexUnlim {
			t.Errorf("entitle %s: HasFlexUnlim true", e.ID)
		}
		if !e.Ent.IsProVeo3Available {
			t.Errorf("entitle %s: IsProVeo3Available false", e.ID)
		}
		if e.Ent.Cohort != "c1" {
			t.Errorf("entitle %s: cohort %q want c1", e.ID, e.Ent.Cohort)
		}
		if e.Ent.TotalPlanCredits != 10000 {
			t.Errorf("entitle %s: total credits %d want 10000", e.ID, e.Ent.TotalPlanCredits)
		}
		wantEnds := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
		if !e.Ent.PlanEndsAt.Equal(wantEnds) {
			t.Errorf("entitle %s: plan_ends_at %s want %s", e.ID, e.Ent.PlanEndsAt, wantEnds)
		}
	}
}

func TestRefresher_ContinuesOnPerAccountFailure(t *testing.T) {
	// Account with marker "bad" gets a 500 on /workspaces/wallet only.
	// /user still succeeds for it, so entitlements land but balance does not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		if strings.Contains(cookie, "bad") && r.URL.Path == "/workspaces/wallet" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userJSON))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeAccountStore{
		accounts: []domain.Account{
			mkAccount("acc_ok_1", "ok1"),
			mkAccount("acc_bad", "bad"),
			mkAccount("acc_ok_2", "ok2"),
		},
	}
	r := newRefresher(t, srv, store, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.tick(ctx)

	// The two healthy accounts should have both wallet + user updates.
	if len(store.balances) != 2 {
		t.Errorf("expected 2 UpdateBalance calls (healthy accounts only), got %d", len(store.balances))
	}
	// All three accounts got a working /user reply → three entitlement writes.
	if len(store.entitles) != 3 {
		t.Errorf("expected 3 UpdateEntitlements calls, got %d", len(store.entitles))
	}
	// Sanity: the failing account's ID must NOT appear in balances.
	for _, b := range store.balances {
		if b.ID == "acc_bad" {
			t.Errorf("acc_bad got a balance write despite wallet 500")
		}
	}
}

func TestRefresher_ConcurrencyBound(t *testing.T) {
	var active int32
	var maxActive int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Track concurrent in-flight handlers. Because the refresher runs
		// each account through a single goroutine that issues wallet then
		// user sequentially, active must never exceed Concurrency.
		n := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if n <= old || atomic.CompareAndSwapInt32(&maxActive, old, n) {
				break
			}
		}
		// Small sleep so parallel goroutines (if the semaphore were
		// broken) would definitely overlap.
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&active, -1)

		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userJSON))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeAccountStore{
		accounts: []domain.Account{
			mkAccount("a1", "m1"),
			mkAccount("a2", "m2"),
			mkAccount("a3", "m3"),
			mkAccount("a4", "m4"),
		},
	}
	r := newRefresher(t, srv, store, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.tick(ctx)

	if got := atomic.LoadInt32(&maxActive); got > 1 {
		t.Errorf("Concurrency=1 but observed %d in-flight requests", got)
	}
	// Sanity: every account was processed.
	if len(store.balances) != 4 {
		t.Errorf("expected 4 balance writes, got %d", len(store.balances))
	}
}

func TestRefresher_TriggerOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userJSON))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &countingAccountStore{
		fakeAccountStore: fakeAccountStore{
			accounts: []domain.Account{
				mkAccount("acc_1", "m1"),
			},
		},
	}
	r := newRefresher(t, srv, store, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.TriggerOnce(ctx)

	if got := atomic.LoadInt32(&store.listCalls); got != 1 {
		t.Fatalf("expected AccountStore.List to be called once, got %d", got)
	}
	if len(store.balances) != 1 {
		t.Fatalf("expected 1 UpdateBalance call, got %d", len(store.balances))
	}
	if len(store.entitles) != 1 {
		t.Fatalf("expected 1 UpdateEntitlements call, got %d", len(store.entitles))
	}
}

// countingAccountStore wraps fakeAccountStore to count List invocations, so
// TestRefresher_TriggerOnce can assert exactly one tick executed.
type countingAccountStore struct {
	fakeAccountStore
	listCalls int32
}

func (c *countingAccountStore) List(ctx context.Context, filter ports.AccountFilter) ([]domain.Account, error) {
	atomic.AddInt32(&c.listCalls, 1)
	return c.fakeAccountStore.List(ctx, filter)
}

func TestRefresher_HandlesEmptyPool(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := &fakeAccountStore{} // no accounts
	r := newRefresher(t, srv, store, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Should return immediately without panicking or hitting the server.
	done := make(chan struct{})
	go func() {
		r.tick(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("tick blocked with empty pool")
	}

	if atomic.LoadInt32(&hits) != 0 {
		t.Errorf("empty pool made %d HTTP requests, want 0", hits)
	}
	if len(store.balances) != 0 || len(store.entitles) != 0 {
		t.Errorf("empty pool triggered store writes: balances=%d entitles=%d",
			len(store.balances), len(store.entitles))
	}
}

// fakeFailoverSink captures RecordError calls so tests can assert
// refresher-observed failures are forwarded to the failover
// controller. Kept minimal — a real controller lives in
// internal/core/failover.
type fakeFailoverSink struct {
	mu    sync.Mutex
	calls []failoverCall
}

type failoverCall struct {
	accountID string
	err       error
	body      string
}

func (f *fakeFailoverSink) RecordError(_ context.Context, accountID string, err error, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, failoverCall{accountID: accountID, err: err, body: body})
}

// TestRefresher_ForwardsFailuresToFailover verifies the Step 1
// failover wiring: when a wallet or /user fetch fails (real production
// signal is HTTP 401 from mint-jwt when a Clerk session has expired),
// the refresher calls Failover.RecordError so the consecutive-fail
// threshold can eventually pause the account. The test uses a
// 500-returning server for one account and a healthy server for two
// others; it asserts exactly two RecordError calls (one for wallet,
// one for /user) against the bad account and zero against the healthy
// ones.
func TestRefresher_ForwardsFailuresToFailover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "bad") {
			// Every endpoint 500s for the bad account so both wallet
			// AND /user fail, hitting both RecordError call sites.
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/workspaces/wallet":
			_, _ = w.Write([]byte(walletJSON))
		case "/user":
			_, _ = w.Write([]byte(userJSON))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	store := &fakeAccountStore{
		accounts: []domain.Account{
			mkAccount("acc_ok_1", "ok1"),
			mkAccount("acc_bad", "bad"),
			mkAccount("acc_ok_2", "ok2"),
		},
	}
	r := newRefresher(t, srv, store, 3)
	fake := &fakeFailoverSink{}
	r.Failover = fake

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r.tick(ctx)

	// Two calls for acc_bad (wallet fail + user fail), zero for the
	// healthy accounts.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	byAcct := map[string]int{}
	for _, c := range fake.calls {
		byAcct[c.accountID]++
	}
	if byAcct["acc_bad"] != 2 {
		t.Errorf("acc_bad: expected 2 RecordError (wallet+user), got %d (all calls: %+v)",
			byAcct["acc_bad"], fake.calls)
	}
	if byAcct["acc_ok_1"] != 0 || byAcct["acc_ok_2"] != 0 {
		t.Errorf("healthy accounts got RecordError: %+v", byAcct)
	}
}

// TestRefresher_NilFailoverIsSafe confirms the failover wiring is
// nil-safe — deployments that leave Failover unset (or [failover].
// enabled=false in main.go) must not panic when a fetch fails.
func TestRefresher_NilFailoverIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := &fakeAccountStore{
		accounts: []domain.Account{mkAccount("acc_solo", "any")},
	}
	r := newRefresher(t, srv, store, 1)
	// r.Failover deliberately left nil.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Must not panic.
	r.tick(ctx)
}
