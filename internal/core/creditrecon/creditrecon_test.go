package creditrecon

// Tests for the credit-ledger reconciler tick. A real upstream.Client is
// pointed at an httptest server serving the /workspaces/credit-ledger/
// statistics endpoint; a fake account store returns one or more active
// accounts; a fake usage store returns per-account local sums; a fake
// notifier records alerts. Coverage:
//   - diff within threshold → no alert
//   - diff over threshold → Warn alert with the expected tags
//   - two consecutive ticks for the same month → alert only once (dedup)
//   - upstream failure on one account → other accounts still processed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeStore embeds ports.AccountStore (nil) and overrides only List — the
// reconciler touches nothing else, so a call to any other method would
// panic, which is the desired tripwire if the deps ever grow silently.
type fakeStore struct {
	ports.AccountStore
	accounts []domain.Account
}

func (f *fakeStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	return f.accounts, nil
}

// fakeUsageStore satisfies ports.UsageEventStore for the one method the
// reconciler uses — SumChargedCreditsHForAccount. Insert/Query/Aggregate
// panic so an accidental call from the tick surface breaks the test.
type fakeUsageStore struct {
	mu     sync.Mutex
	sums   map[string]int64 // accountID -> local _h to return
	errors map[string]error // accountID -> error to return (optional)
	calls  int
}

func (f *fakeUsageStore) Insert(context.Context, *domain.UsageEvent) error {
	panic("not implemented")
}
func (f *fakeUsageStore) Query(context.Context, ports.UsageQuery) ([]domain.UsageEvent, error) {
	panic("not implemented")
}
func (f *fakeUsageStore) Aggregate(context.Context, ports.UsageAggQuery) ([]ports.UsageAggRow, error) {
	panic("not implemented")
}
func (f *fakeUsageStore) SumChargedCreditsHForAccount(_ context.Context, accountID string, _, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err, ok := f.errors[accountID]; ok {
		return 0, err
	}
	return f.sums[accountID], nil
}

// fakeNotifier records sent notifications.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []ports.Notification
}

func (f *fakeNotifier) Name() string { return "fake" }
func (f *fakeNotifier) Send(_ context.Context, m ports.Notification) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}
func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}
func (f *fakeNotifier) last() ports.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ports.Notification{}
	}
	return f.sent[len(f.sent)-1]
}

// fakeHTTPClient short-circuits the clerk JWT mint and forwards everything
// else to the real transport (so the httptest server serves the credit
// ledger endpoint).
type fakeHTTPClient struct{ mintJWT string }

func (f *fakeHTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"jwt":%q}`, f.mintJWT))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return http.DefaultClient.Do(req.WithContext(ctx))
}
func (f *fakeHTTPClient) Fingerprint() string { return "fake" }
func (f *fakeHTTPClient) Name() string        { return "fake" }

func newFakeJWT() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"sub": "user_test", "email": "t@example.com",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

func mkAccount(id string) domain.Account {
	return domain.Account{
		ID: id, Email: id + "@example.com", SessionID: "sess_" + id,
		CookiesJSON: `{"__session":"stub"}`, UserAgent: "test", Status: domain.StatusActive,
	}
}

// ledgerHandler maps accountID (deduced from the mint JWT's sub or, when
// the client uses a single fake token, from a per-request state) to a
// canned upstream statistics response. Because our fakeHTTPClient mints
// the same JWT for every account, we key on a rotating counter so tests
// with multiple accounts can still return distinct responses.
type ledgerHandler struct {
	mu       sync.Mutex
	byOrder  []string // ordered list of JSON bodies to return, one per call
	statuses []int    // parallel HTTP statuses (0 → 200)
	calls    int
	lastURL  string
}

func (h *ledgerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/workspaces/credit-ledger/statistics" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastURL = r.URL.String()
	idx := h.calls
	h.calls++
	if idx >= len(h.byOrder) {
		// Fall back to a benign zero response so tests that don't
		// exercise every account get a predictable "no upstream spend".
		_, _ = w.Write([]byte(`{}`))
		return
	}
	status := 200
	if idx < len(h.statuses) && h.statuses[idx] != 0 {
		status = h.statuses[idx]
	}
	if status != 200 {
		w.WriteHeader(status)
	}
	_, _ = w.Write([]byte(h.byOrder[idx]))
}

func newReconciler(t *testing.T, srv *httptest.Server, store ports.AccountStore, usage ports.UsageEventStore, ntf ports.Notifier) *Reconciler {
	t.Helper()
	fake := &fakeHTTPClient{mintJWT: newFakeJWT()}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	up := upstream.New(fake, minter, upstream.Config{BaseURL: srv.URL})
	r := New(store, usage, up, ntf, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Concurrency = 1                    // deterministic order in ledger handler
	r.now = func() time.Time {           // fixed clock: reconciles June 2026
		return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	}
	return r
}

func TestCreditRecon_WithinThreshold_NoAlert(t *testing.T) {
	// Upstream reports 1000 credits (= 100_000 _h). Local sum is 99_500 _h
	// (diff 500 _h, well under the 10_000 _h absolute floor). Expect no
	// alert.
	h := &ledgerHandler{byOrder: []string{`{"total_credits_spent":1000,"total_credits_refunded":0,"jobs_created":5,"currency":"USD"}`}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}
	usage := &fakeUsageStore{sums: map[string]int64{"acc_1": 99_500}}
	ntf := &fakeNotifier{}

	r := newReconciler(t, srv, store, usage, ntf)
	r.TriggerOnce(context.Background())

	if got := ntf.count(); got != 0 {
		t.Errorf("expected 0 alerts within threshold, got %d (%+v)", got, ntf.sent)
	}
	if h.calls != 1 {
		t.Errorf("expected 1 upstream call, got %d", h.calls)
	}
	if usage.calls != 1 {
		t.Errorf("expected 1 local sum call, got %d", usage.calls)
	}
	// Sanity-check the query window landed on June 2026.
	if !strings.Contains(h.lastURL, "start_date=2026-06-01") {
		t.Errorf("expected start_date=2026-06-01 in URL, got %s", h.lastURL)
	}
}

func TestCreditRecon_OverThreshold_AlertsWithTags(t *testing.T) {
	// Upstream 1000 credits → 100_000 _h. Local 50_000 _h → diff 50_000 _h,
	// way over both the 10_000 _h absolute floor and the 5_000 _h relative
	// (5% of 100_000). Expect one Warn alert whose Tags carry the raw
	// upstream/local numbers.
	h := &ledgerHandler{byOrder: []string{`{"total_credits_spent":1000,"total_credits_refunded":0,"jobs_created":5,"currency":"USD"}`}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}
	usage := &fakeUsageStore{sums: map[string]int64{"acc_1": 50_000}}
	ntf := &fakeNotifier{}

	r := newReconciler(t, srv, store, usage, ntf)
	r.TriggerOnce(context.Background())

	if got := ntf.count(); got != 1 {
		t.Fatalf("expected 1 alert over threshold, got %d", got)
	}
	got := ntf.last()
	if got.Level != ports.LevelWarn {
		t.Errorf("expected LevelWarn, got %q", got.Level)
	}
	// Tags: caller must be able to read raw upstream/local numbers.
	assertTag(t, got, "account_id", "acc_1")
	assertTag(t, got, "upstream_credits", "1000")
	assertTag(t, got, "local_credits", "500")
	assertTag(t, got, "upstream_h", "100000")
	assertTag(t, got, "local_h", "50000")
	assertTag(t, got, "diff_h", "50000")
	assertTag(t, got, "month", "2026-06")
}

func TestCreditRecon_DedupSameMonth(t *testing.T) {
	// Two consecutive ticks with the same over-threshold state must only
	// produce one alert — dedup key is (account, target-month).
	h := &ledgerHandler{byOrder: []string{
		`{"total_credits_spent":1000,"total_credits_refunded":0,"jobs_created":5,"currency":"USD"}`,
		`{"total_credits_spent":1000,"total_credits_refunded":0,"jobs_created":5,"currency":"USD"}`,
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}
	usage := &fakeUsageStore{sums: map[string]int64{"acc_1": 50_000}}
	ntf := &fakeNotifier{}

	r := newReconciler(t, srv, store, usage, ntf)
	r.TriggerOnce(context.Background())
	r.TriggerOnce(context.Background())

	if got := ntf.count(); got != 1 {
		t.Errorf("expected 1 alert across 2 ticks (same month), got %d", got)
	}
}

func TestCreditRecon_UpstreamFailure_DoesNotBlockOtherAccounts(t *testing.T) {
	// Two accounts. The first upstream call returns 500 (fails); the
	// second returns a mismatched aggregate. Expect the first to be
	// skipped and the second to alert normally.
	h := &ledgerHandler{
		byOrder:  []string{`{"error":"boom"}`, `{"total_credits_spent":1000,"total_credits_refunded":0,"jobs_created":5,"currency":"USD"}`},
		statuses: []int{500, 200},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Order matters — the fanout is sequential (Concurrency=1) so
	// account[0] gets the 500 and account[1] gets the mismatched 200.
	store := &fakeStore{accounts: []domain.Account{mkAccount("acc_bad"), mkAccount("acc_good")}}
	usage := &fakeUsageStore{sums: map[string]int64{"acc_bad": 999, "acc_good": 50_000}}
	ntf := &fakeNotifier{}

	r := newReconciler(t, srv, store, usage, ntf)
	r.TriggerOnce(context.Background())

	if got := ntf.count(); got != 1 {
		t.Fatalf("expected 1 alert (for acc_good only), got %d", got)
	}
	assertTag(t, ntf.last(), "account_id", "acc_good")
}

// Ensure a local-store failure on one account also doesn't block other
// accounts (mirrors the upstream failure case from the other side).
func TestCreditRecon_LocalStoreFailure_DoesNotBlockOthers(t *testing.T) {
	h := &ledgerHandler{byOrder: []string{
		`{"total_credits_spent":1000,"jobs_created":5,"currency":"USD"}`,
		`{"total_credits_spent":1000,"jobs_created":5,"currency":"USD"}`,
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := &fakeStore{accounts: []domain.Account{mkAccount("acc_bad"), mkAccount("acc_good")}}
	usage := &fakeUsageStore{
		sums:   map[string]int64{"acc_good": 50_000},
		errors: map[string]error{"acc_bad": errors.New("db down")},
	}
	ntf := &fakeNotifier{}

	r := newReconciler(t, srv, store, usage, ntf)
	r.TriggerOnce(context.Background())

	if got := ntf.count(); got != 1 {
		t.Fatalf("expected 1 alert (for acc_good only), got %d", got)
	}
	assertTag(t, ntf.last(), "account_id", "acc_good")
}

// TestCreditRecon_TargetWindow_FirstOfMonth checks that reconciling on the
// 1st of a month picks the previous month's window, not the current one.
func TestCreditRecon_TargetWindow_FirstOfMonth(t *testing.T) {
	h := &ledgerHandler{byOrder: []string{`{}`}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	store := &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}
	usage := &fakeUsageStore{}
	ntf := &fakeNotifier{}

	r := newReconciler(t, srv, store, usage, ntf)
	r.now = func() time.Time { return time.Date(2026, 7, 1, 0, 15, 0, 0, time.UTC) }
	r.TriggerOnce(context.Background())

	// Expect the URL to name June (the just-completed month), not July.
	if !strings.Contains(h.lastURL, "start_date=2026-06-01") {
		t.Errorf("expected 2026-06-01 window on July 1st tick, got URL %s", h.lastURL)
	}
	if !strings.Contains(h.lastURL, "end_date=2026-06-30") {
		t.Errorf("expected end_date=2026-06-30 (inclusive, last day of June), got URL %s", h.lastURL)
	}
}

func TestCreditRecon_NilDeps_ReturnsImmediately(t *testing.T) {
	// A Reconciler with nil deps should not panic; Run must return
	// promptly. Guard against an accidental nil-deref if wiring is
	// partial (e.g., notifier chain not wired).
	r := &Reconciler{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.TriggerOnce(context.Background()) // no panic
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx) // returns immediately due to nil deps guard
}

func assertTag(t *testing.T, n ports.Notification, key, want string) {
	t.Helper()
	got, ok := n.Tags[key]
	if !ok {
		t.Errorf("missing tag %q in notification %+v", key, n)
		return
	}
	if got != want {
		t.Errorf("tag %q: got %q, want %q", key, got, want)
	}
}
