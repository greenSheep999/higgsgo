package utls

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Pool builds one *Client per unique proxy URL, caching them so repeat
// requests for the same account reuse HTTP/2 connections. Missing key
// = no proxy (the "shared / global" client). Thread-safe under
// concurrent Resolve calls.
//
// This is the runtime piece of ROADMAP P1-5: it makes
// account.bound_proxy_url actually affect outbound traffic. Before this,
// every account shared one process-level client and therefore one
// egress IP, breaking the sticky-IP guarantee we make to Higgsfield at
// registration time and inviting Cloudflare / DataDome to correlate
// multiple accounts to the same source.
type Pool struct {
	profile string
	timeout Config // captured for reuse (profile + timeout)

	// defaultClient is what "no override" resolves to. Typically the
	// process-level client built from HIGGSGO_UPSTREAM_PROXY_URL (or
	// direct if unset). Never nil after NewPool returns.
	defaultClient *Client

	mu      sync.Mutex
	byProxy map[string]*Client // keyed by proxy URL
	// buildErrs remembers the first build failure per URL so we don't
	// hammer a broken proxy — a failed URL sticks to the fallback for
	// the process lifetime. Cleared implicitly when Invalidate is
	// called.
	buildErrs map[string]error
}

// NewPool constructs a Pool wrapped around a default Client. The default
// is used for accounts with an empty BoundProxyURL, and as fallback
// when a per-account URL is malformed. The pool captures cfg's Profile
// and Timeout so per-URL clients are built with the same fingerprint.
func NewPool(cfg Config, defaultClient *Client) *Pool {
	// The pool stores cfg for building new per-proxy clients; strip its
	// ProxyURL because that field is the one we override.
	tpl := Config{Profile: cfg.Profile, Timeout: cfg.Timeout}
	return &Pool{
		profile:       cfg.Profile,
		timeout:       tpl,
		defaultClient: defaultClient,
		byProxy:       map[string]*Client{},
		buildErrs:     map[string]error{},
	}
}

// Resolve implements upstream.AccountClientResolver: returns the
// UpstreamClient for the given account. Empty account.BoundProxyURL
// returns nil so upstream.Client falls back to its default (which is
// this pool's default — same object, single indirection).
//
// A malformed or previously-broken proxy URL is logged once, remembered
// in buildErrs, and future calls for that URL return nil (fallback)
// without retrying the build.
func (p *Pool) Resolve(_ context.Context, account *domain.Account) (ports.UpstreamClient, error) {
	if account == nil {
		return nil, nil
	}
	proxyURL := account.BoundProxyURL
	if proxyURL == "" {
		return nil, nil // caller uses upstream.Client.http (== defaultClient)
	}
	c, err := p.get(proxyURL)
	if err != nil {
		// Cached / new build failure — upstream.Client falls back to
		// its default. Returning the error lets the upstream layer log
		// it once.
		return nil, err
	}
	return c, nil
}

// DefaultClient exposes the fallback client so main.go can hand the
// same instance to upstream.New. Keeping one shared default keeps the
// hot path (empty bound_proxy_url) allocation-free.
func (p *Pool) DefaultClient() *Client { return p.defaultClient }

// Invalidate drops any cached client for the given URL. Call this when
// a proxy has been observed to fail so the next Resolve rebuilds a
// fresh transport (typically the failover controller will do this
// after marking an account throttled/disabled and the operator
// re-binds a different URL). No-op on unknown keys.
func (p *Pool) Invalidate(proxyURL string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byProxy, proxyURL)
	delete(p.buildErrs, proxyURL)
}

func (p *Pool) get(proxyURL string) (*Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.byProxy[proxyURL]; ok {
		return c, nil
	}
	if err, ok := p.buildErrs[proxyURL]; ok {
		return nil, err
	}
	cfg := p.timeout
	cfg.ProxyURL = proxyURL
	c, err := New(cfg)
	if err != nil {
		wrapped := fmt.Errorf("utls pool: build client for %q: %w", proxyURL, err)
		p.buildErrs[proxyURL] = wrapped
		return nil, wrapped
	}
	p.byProxy[proxyURL] = c
	return c, nil
}

// ErrPoolClosed is returned by Resolve after Close (currently unused;
// reserved so future graceful-shutdown code has a stable sentinel).
var ErrPoolClosed = errors.New("utls pool: closed")
