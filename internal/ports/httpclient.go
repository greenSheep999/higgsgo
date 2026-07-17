package ports

import (
	"context"
	"net/http"
)

// UpstreamClient sends HTTP requests to higgsfield.ai (fnf.higgsfield.ai +
// clerk.higgsfield.ai). It must present a JA3/TLS fingerprint that matches
// a real Chrome build — the vanilla net/http fingerprint is blocked by
// Cloudflare on this domain.
//
// Adapters: utls, mimic, cycletls, or a bridge to a Node impit subprocess.
type UpstreamClient interface {
	// Do sends req and returns the response. The client is responsible for
	// applying the JA3 fingerprint, honoring the request's context, and
	// following redirects per the adapter's configuration.
	Do(ctx context.Context, req *http.Request) (*http.Response, error)

	// Fingerprint is a short human-readable identifier of the TLS profile
	// in use (e.g. "chrome_133"). Included in metrics labels.
	Fingerprint() string

	Name() string
}
