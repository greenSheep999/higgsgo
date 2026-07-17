package ports

import "context"

// ProxyProvider issues SOCKS5 (or HTTP) proxies for outbound higgsfield calls.
// Concrete adapters may pull from a static file (711proxy list), a rotating
// residential provider (BrightData), or return a no-op for local testing.
type ProxyProvider interface {
	// Acquire returns a proxy usable for a single outbound call chain.
	// When opts.Sticky is true, the provider is expected to return the same
	// URL for future calls with the same ForAccountID.
	Acquire(ctx context.Context, opts ProxyOpts) (Proxy, error)

	// Release marks the proxy as no longer in active use. Sticky proxies are
	// not actually returned to the pool; this call only updates last_used_at.
	Release(ctx context.Context, p Proxy) error

	// Healthcheck probes the proxy with a small request. Returns nil if healthy.
	Healthcheck(ctx context.Context, p Proxy) error

	// Name identifies the provider for logs and metrics ("711proxy", "static", ...).
	Name() string
}

// Proxy is a single outbound proxy assignment.
type Proxy struct {
	URL      string            // "socks5://user:pass@host:port"
	Region   string            // "US" / "VN" / "IN" / ...
	Provider string            // provider name
	Sticky   bool              // true if bound long-term to an account
	Metadata map[string]string // provider-specific extras
}

// ProxyOpts describes what kind of proxy the caller wants.
type ProxyOpts struct {
	Region       string // empty means any region
	Sticky       bool   // whether to bind long-term (registration flow needs this)
	ForAccountID string // required when Sticky is true
	MinLatencyMS int    // 0 means no requirement
}
