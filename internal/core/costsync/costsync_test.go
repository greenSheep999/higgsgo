package costsync

// Tests for the costsync tick: a real upstream.Client points at an
// httptest server serving GET /job-sets/costs; a fake store returns one
// active account; a fake CostSink records what got pushed. Assertions
// cover: costs are fetched + pushed, no-account is a warn no-op, and an
// upstream failure leaves the sink untouched.

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
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeStore embeds ports.AccountStore (nil) and overrides only List — the
// syncer touches nothing else, so any other call panics as a tripwire.
type fakeStore struct {
	ports.AccountStore
	accounts []domain.Account
}

func (f *fakeStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	return f.accounts, nil
}

// fakeSink records the last SetDynamicCosts payload.
type fakeSink struct {
	mu    sync.Mutex
	last  map[string]int64
	calls int
}

type fakePricingStore struct {
	ports.PricingStore
	mu       sync.Mutex
	snapshot *domain.PricingSnapshot
	rules    []domain.ModelCostRule
	err      error
}

func (f *fakePricingStore) SaveSnapshot(_ context.Context, snapshot *domain.PricingSnapshot, rules []domain.ModelCostRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.snapshot = snapshot
	f.rules = append([]domain.ModelCostRule(nil), rules...)
	return nil
}

func (s *fakeSink) SetDynamicCosts(costs map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = costs
	s.calls++
}

func (s *fakeSink) snapshot() (map[string]int64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.calls
}

// fakeHTTPClient short-circuits the clerk JWT mint and forwards everything
// else to the real transport (so the httptest server serves the costs).
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

func newSyncer(srv *httptest.Server, store ports.AccountStore, sink CostSink) *Syncer {
	fake := &fakeHTTPClient{mintJWT: newFakeJWT()}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	up := upstream.New(fake, minter, upstream.Config{BaseURL: srv.URL})
	return &Syncer{
		Accounts: store,
		Upstream: up,
		Registry: sink,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

const costsFixture = `{"data":[
  {"job_set_type":"kling3_0","cost":[{"mode":"pro","audio":{"on":2.5,"off":1.75}}]},
  {"job_set_type":"seedance_2_0_mini","cost":[{"resolution":"480p","cost_per_second":1,"original_cost_per_second":1}]}
]}`

func TestCostsync_FetchesAndPushes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/job-sets/costs" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(costsFixture))
	}))
	defer srv.Close()

	sink := &fakeSink{}
	s := newSyncer(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, sink)
	s.TriggerOnce(context.Background())

	last, calls := sink.snapshot()
	if calls != 1 {
		t.Fatalf("SetDynamicCosts calls = %d, want 1", calls)
	}
	if last["kling3_0"] != 175 { // min(2.5,1.75) = 1.75
		t.Errorf("kling3_0 = %d, want 175", last["kling3_0"])
	}
	if last["seedance_2_0_mini"] != 100 {
		t.Errorf("seedance_2_0_mini = %d, want 100", last["seedance_2_0_mini"])
	}
}

func TestCostsync_PersistsBeforePublishingOverlay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(costsFixture))
	}))
	defer srv.Close()

	sink := &fakeSink{}
	pricing := &fakePricingStore{}
	s := newSyncer(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, sink)
	s.Pricing = pricing
	s.TriggerOnce(context.Background())

	if pricing.snapshot == nil {
		t.Fatal("pricing snapshot was not persisted")
	}
	if pricing.snapshot.Source != "higgs_job_set_costs" || pricing.snapshot.PayloadJSON == "" || pricing.snapshot.PayloadSHA256 == "" {
		t.Fatalf("incomplete persisted snapshot: %+v", pricing.snapshot)
	}
	if len(pricing.rules) != 3 {
		t.Fatalf("persisted rule count = %d, want 3; rules=%+v", len(pricing.rules), pricing.rules)
	}
	for _, rule := range pricing.rules {
		if rule.ID == "" || rule.SnapshotID != pricing.snapshot.ID || rule.ObservedAt.IsZero() {
			t.Fatalf("rule persistence metadata incomplete: %+v", rule)
		}
	}
	if _, calls := sink.snapshot(); calls != 1 {
		t.Fatalf("SetDynamicCosts calls = %d, want 1 after persistence", calls)
	}
}

func TestCostsync_PersistenceFailureLeavesOverlayUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(costsFixture))
	}))
	defer srv.Close()

	sink := &fakeSink{}
	s := newSyncer(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, sink)
	s.Pricing = &fakePricingStore{err: fmt.Errorf("database unavailable")}
	s.TriggerOnce(context.Background())

	if _, calls := sink.snapshot(); calls != 0 {
		t.Fatalf("SetDynamicCosts calls = %d, want 0 after persistence failure", calls)
	}
}

func TestCostsync_NoActiveAccount_NoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called with zero accounts")
	}))
	defer srv.Close()

	sink := &fakeSink{}
	s := newSyncer(srv, &fakeStore{accounts: nil}, sink)
	s.TriggerOnce(context.Background())

	if _, calls := sink.snapshot(); calls != 0 {
		t.Errorf("SetDynamicCosts calls = %d, want 0", calls)
	}
}

func TestCostsync_UpstreamError_LeavesSinkUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	sink := &fakeSink{}
	s := newSyncer(srv, &fakeStore{accounts: []domain.Account{mkAccount("a1")}}, sink)
	s.TriggerOnce(context.Background())

	if _, calls := sink.snapshot(); calls != 0 {
		t.Errorf("SetDynamicCosts calls = %d on upstream error, want 0", calls)
	}
}
