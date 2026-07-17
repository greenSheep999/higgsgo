// Package utls implements UpstreamClient using refraction-networking/utls
// to present a Chrome TLS fingerprint (JA3) that Cloudflare + DataDome will
// accept. Required for any real higgsfield.ai traffic.
//
// The transport is a modified http.Transport that dials TLS via utls
// instead of crypto/tls, using a profile chosen by config.
package utls

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// Client implements ports.UpstreamClient with a Chrome TLS fingerprint.
type Client struct {
	inner   *http.Client
	profile string
}

// Config controls Client construction.
type Config struct {
	// Profile picks the ClientHello. Currently supported: "chrome_133",
	// "chrome_120", "chrome_100". Empty defaults to "chrome_133".
	Profile string

	// ProxyURL, when non-empty, tunnels every request through the given
	// socks5:// or http:// proxy.
	ProxyURL string

	// Timeout is the per-request total timeout. Zero disables it.
	Timeout time.Duration
}

// New builds a Client. Returns an error when the profile name is not known
// or when the proxy URL is malformed.
func New(cfg Config) (*Client, error) {
	profile := cfg.Profile
	if profile == "" {
		profile = "chrome_133"
	}
	helloID, err := parseProfile(profile)
	if err != nil {
		return nil, err
	}

	// Base dialer (direct or proxied).
	var baseDialer func(ctx context.Context, network, addr string) (net.Conn, error)
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy url: %w", err)
		}
		switch u.Scheme {
		case "socks5", "socks5h":
			d, err := proxy.FromURL(u, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("socks5 dialer: %w", err)
			}
			baseDialer = func(_ context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		case "http", "https":
			// http proxy — we tunnel via CONNECT and then wrap with utls.
			baseDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return httpProxyDial(ctx, u, network, addr)
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	} else {
		nd := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		baseDialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nd.DialContext(ctx, network, addr)
		}
	}

	// TLS dialer: performs utls handshake with h2 + http/1.1 ALPN.
	// Returns either an HTTP/2 connection wrapped by http2.Transport or a
	// plain utls.UConn for HTTP/1.1.
	h2Transport := &http2.Transport{
		AllowHTTP: false,
	}
	h1Transport := &http.Transport{
		DialContext:           baseDialer,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	dialAndUpgrade := func(ctx context.Context, network, addr string) (net.Conn, string, error) {
		conn, err := baseDialer(ctx, network, addr)
		if err != nil {
			return nil, "", err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		uconn := utls.UClient(conn, &utls.Config{
			ServerName: host,
			NextProtos: []string{"h2", "http/1.1"},
		}, helloID)
		if err := uconn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, "", fmt.Errorf("utls handshake: %w", err)
		}
		return uconn, uconn.ConnectionState().NegotiatedProtocol, nil
	}

	// http2.Transport uses its own DialTLS. We reuse dialAndUpgrade.
	h2Transport.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		conn, proto, err := dialAndUpgrade(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if proto != "h2" {
			_ = conn.Close()
			return nil, fmt.Errorf("expected h2 ALPN, got %q", proto)
		}
		return conn, nil
	}

	// http1 fallback DialTLS for downgraded servers.
	h1Transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, _, err := dialAndUpgrade(ctx, network, addr)
		return conn, err
	}

	// Router: pick h2 by default for higgsfield's known-h2 domains, fall
	// back to h1 otherwise. Simpler than probing ALPN on every request.
	rt := &protoRouter{h1: h1Transport, h2: h2Transport}

	return &Client{
		inner: &http.Client{
			Transport: rt,
			Timeout:   cfg.Timeout,
		},
		profile: profile,
	}, nil
}

// protoRouter picks between HTTP/1.1 and HTTP/2 transports based on the
// scheme and host. For https+known-h2 hosts it uses h2; else h1.
type protoRouter struct {
	h1 *http.Transport
	h2 *http2.Transport
}

func (r *protoRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		return r.h2.RoundTrip(req)
	}
	return r.h1.RoundTrip(req)
}

// Do sends the request through the utls-tunneled transport.
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return c.inner.Do(req.WithContext(ctx))
}

// Fingerprint returns the profile name in use (e.g. "chrome_133").
func (c *Client) Fingerprint() string { return c.profile }

// Name returns the adapter name.
func (c *Client) Name() string { return "utls" }

// parseProfile maps a config string to a utls.ClientHelloID.
func parseProfile(name string) (utls.ClientHelloID, error) {
	switch strings.ToLower(name) {
	case "chrome_133", "chrome":
		return utls.HelloChrome_133, nil
	case "chrome_120":
		return utls.HelloChrome_120, nil
	case "chrome_100":
		return utls.HelloChrome_100, nil
	case "chrome_102":
		return utls.HelloChrome_102, nil
	case "chrome_106":
		return utls.HelloChrome_106_Shuffle, nil
	case "firefox", "firefox_120":
		return utls.HelloFirefox_120, nil
	case "firefox_105":
		return utls.HelloFirefox_105, nil
	case "safari", "safari_16":
		return utls.HelloSafari_16_0, nil
	}
	return utls.ClientHelloID{}, fmt.Errorf("unknown utls profile %q", name)
}

// httpProxyDial performs a CONNECT tunnel through an http/https proxy and
// returns the raw TCP conn ready for TLS handshake.
func httpProxyDial(ctx context.Context, proxyURL *url.URL, network, addr string) (net.Conn, error) {
	if network != "tcp" && !strings.HasPrefix(network, "tcp") {
		return nil, errors.New("utls http proxy: only tcp supported")
	}
	nd := &net.Dialer{Timeout: 30 * time.Second}
	conn, err := nd.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u := proxyURL.User; u != nil {
		user := u.Username()
		pass, _ := u.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+basicAuth(user, pass))
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Read minimal response line: "HTTP/1.1 200 ..."
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	head := string(buf[:n])
	if !strings.HasPrefix(head, "HTTP/1.1 200") && !strings.HasPrefix(head, "HTTP/1.0 200") {
		_ = conn.Close()
		firstLine := strings.SplitN(head, "\r\n", 2)[0]
		return nil, fmt.Errorf("proxy CONNECT failed: %s", firstLine)
	}
	return conn, nil
}

func basicAuth(user, pass string) string {
	// Small inline replacement to avoid importing encoding/base64 here for
	// a single call; using stdlib is fine, so just do that.
	return base64Std(user + ":" + pass)
}
