package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestClient_Resolver_RoutesPerAccountClient covers ROADMAP P1-5's key
// invariant: when a per-account resolver returns a non-nil client for
// an account, that client's Do is called instead of the shared default.
// Verified by wiring two counting clients (one as default, one as the
// resolver's return) and asserting the resolver's client received the
// request.
func TestClient_Resolver_RoutesPerAccountClient(t *testing.T) {
	// Minimal http server so buildReq has a URL to hit; the actual
	// transport is the counting client, not net/http, so the server
	// never receives anything.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defaultClient := newCountingMintFake(t)
	perAccountClient := newCountingMintFake(t)

	minter := jwt.New(defaultClient, ports.RealClock{}, jwt.Config{})
	c := New(defaultClient, minter, Config{BaseURL: srv.URL})
	c.Resolver = &stubResolver{
		pick: func(a *domain.Account) ports.UpstreamClient {
			if a.BoundProxyURL != "" {
				return perAccountClient
			}
			return nil
		},
	}

	// Account WITHOUT a bound proxy: resolver returns nil, request
	// falls through to the default client.
	acc1 := &domain.Account{ID: "acc_default", SessionID: "-", CookiesJSON: "{}", UserAgent: "-"}
	if _, err := c.FetchWallet(context.Background(), acc1); err != nil {
		t.Fatalf("acc_default FetchWallet: %v", err)
	}
	if got := defaultClient.otherCalls.Load(); got != 1 {
		t.Errorf("default client should have handled acc_default; got %d non-mint calls", got)
	}
	if got := perAccountClient.otherCalls.Load(); got != 0 {
		t.Errorf("per-account client must not receive default account traffic; got %d", got)
	}

	// Account WITH a bound proxy: resolver returns the per-account
	// client, request egresses through it.
	acc2 := &domain.Account{
		ID: "acc_proxied", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		BoundProxyURL: "socks5://prox:pass@10.0.0.1:1080",
	}
	if _, err := c.FetchWallet(context.Background(), acc2); err != nil {
		t.Fatalf("acc_proxied FetchWallet: %v", err)
	}
	if got := perAccountClient.otherCalls.Load(); got != 1 {
		t.Errorf("per-account client should have handled acc_proxied; got %d", got)
	}
}

// TestClient_Resolver_FallsBackOnResolverError verifies that a resolver
// error does NOT bubble up to the caller — upstream.Client logs the
// warning and uses the default client. Requests must not fail just
// because a per-account proxy is misconfigured.
func TestClient_Resolver_FallsBackOnResolverError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defaultClient := newCountingMintFake(t)
	minter := jwt.New(defaultClient, ports.RealClock{}, jwt.Config{})
	c := New(defaultClient, minter, Config{BaseURL: srv.URL})
	c.Resolver = &stubResolver{err: errors.New("proxy pool broken")}

	acc := &domain.Account{ID: "acc_broken", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		BoundProxyURL: "socks5://never-resolves"}
	if _, err := c.FetchWallet(context.Background(), acc); err != nil {
		t.Fatalf("FetchWallet should succeed via fallback: %v", err)
	}
	if got := defaultClient.otherCalls.Load(); got != 1 {
		t.Errorf("fallback path should call default client once; got %d", got)
	}
}

// stubResolver implements AccountClientResolver for tests.
type stubResolver struct {
	pick func(*domain.Account) ports.UpstreamClient
	err  error
}

func (s *stubResolver) Resolve(_ context.Context, a *domain.Account) (ports.UpstreamClient, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.pick == nil {
		return nil, nil
	}
	return s.pick(a), nil
}

// countingMintFake is a version of countingMintClient that also counts
// non-mint (business-endpoint) calls so tests can assert which of two
// injected clients actually served the upstream call.
type countingMintFake struct {
	countingMintClient
	otherCalls atomic.Int64
	body       string
}

func newCountingMintFake(t *testing.T) *countingMintFake {
	f := &countingMintFake{body: `{"workspace_id":"ws","credits_balance":100}`}
	f.newJWT = func(seq int64) string {
		return newFakeJWTWithSub(t, fmt.Sprintf("user_%d", seq))
	}
	return f
}

func (f *countingMintFake) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		return f.countingMintClient.Do(ctx, req)
	}
	f.otherCalls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}
