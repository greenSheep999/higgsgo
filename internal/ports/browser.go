package ports

import (
	"context"
	"time"
)

// BrowserAutomator drives a real (or Chromium-like) browser during account
// registration. higgsfield.ai's login flow requires JS execution, canvas
// fingerprint stability, and passing DataDome — a plain HTTP client is not
// enough.
//
// Adapters may embed CloakBrowser via a Node subprocess (proven to work),
// pure-Go chromedp / rod, patchright (stealth Chromium), or playwright-go.
type BrowserAutomator interface {
	// Launch starts a browser session. The returned BrowserSession is
	// tied to a single account registration attempt and must be Close()'d.
	Launch(ctx context.Context, opts LaunchOpts) (BrowserSession, error)

	Name() string
}

// LaunchOpts controls per-session browser configuration.
type LaunchOpts struct {
	ProxyURL   string
	UserAgent  string // optional; empty means adapter default
	Headless   bool
	Profile    string   // separate on-disk profile directory
	ExtraFlags []string // adapter-specific Chromium flags
}

// BrowserSession is the interactive surface of a running browser.
type BrowserSession interface {
	Goto(ctx context.Context, url string) error

	// DOM interactions.
	Fill(ctx context.Context, selector, value string) error
	Click(ctx context.Context, selector string) error
	WaitFor(ctx context.Context, selector string, timeout time.Duration) error

	// State capture, invoked after the login flow completes.
	Cookies(ctx context.Context) ([]Cookie, error)
	LocalStorage(ctx context.Context) (map[string]string, error)
	UserAgent(ctx context.Context) (string, error)

	// EvalJS runs a JS snippet and returns the JSON-encodable result.
	EvalJS(ctx context.Context, script string) (any, error)

	// Network interception. Used to sniff Clerk JWTs and DataDome cookies
	// from the response headers during registration.
	OnRequest(cb func(NetRequest))
	OnResponse(cb func(NetResponse))

	Close() error
}

// Cookie is a browser cookie captured after login.
type Cookie struct {
	Name     string
	Value    string
	Domain   string
	Path     string
	Expires  time.Time
	HTTPOnly bool
	Secure   bool
	SameSite string
}

// NetRequest is a browser-emitted HTTP request observed via CDP.
type NetRequest struct {
	URL     string
	Method  string
	Headers map[string]string
}

// NetResponse is a browser-observed HTTP response.
type NetResponse struct {
	URL        string
	Status     int
	Headers    map[string]string
	SetCookies []Cookie
}
