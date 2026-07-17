// Package stdhttp is a straight-through UpstreamClient backed by the Go
// standard net/http package. It is useful for local development against
// mock servers but WILL be blocked by Cloudflare + DataDome on real
// higgsfield endpoints — the TLS fingerprint is not a real browser.
//
// Use the utls adapter for anything that touches fnf.higgsfield.ai.
package stdhttp

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// Client implements ports.UpstreamClient using the standard http.Client.
type Client struct {
	inner *http.Client
}

// Config controls the client.
type Config struct {
	// ProxyURL, when non-empty, forces every outbound request through the
	// given URL. Supports "socks5://user:pass@host:port" and "http://...".
	ProxyURL string

	// Timeout is the total per-request timeout. Zero means no timeout.
	Timeout time.Duration
}

// New constructs a stdhttp Client with the given config.
func New(cfg Config) (*Client, error) {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, err
		}
		switch u.Scheme {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			dialer, err := proxy.FromURL(u, proxy.Direct)
			if err != nil {
				return nil, err
			}
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	}
	return &Client{
		inner: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}, nil
}

// Do sends the request. The context on the request takes precedence over
// the client's Timeout when both are set.
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return c.inner.Do(req.WithContext(ctx))
}

// Fingerprint returns "go_stdhttp" — this client is easily detected.
func (c *Client) Fingerprint() string { return "go_stdhttp" }

// Name returns the adapter name.
func (c *Client) Name() string { return "stdhttp" }
