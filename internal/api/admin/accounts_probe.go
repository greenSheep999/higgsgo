package admin

// Account "probe": a cheap active health check the operator can run
// on demand from the accounts page or CLI. Its purpose is to answer
// "does this account still work?" without waiting for the next
// scheduled refresh or a failing job.
//
// The probe hits GET /workspaces/wallet through the same upstream
// client the pool uses in production — same JA3 fingerprint, same
// per-account proxy (P1-5), same JWT minter. That means a green
// probe result is a strong signal: the account's cookies are alive,
// its bound proxy is reachable, its Clerk session is refreshable,
// and Cloudflare / DataDome are happy with the fingerprint at this
// egress IP. A red result names exactly which layer failed so the
// operator can act on it immediately.
//
// Previously this endpoint did not exist and the WebUI's probe button
// just fired a success toast. See docs/ROADMAP.md P2-6.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// Prober is the narrow subset of the upstream client the probe uses.
// Defined locally so the admin package can be tested with a stub and so
// this file has no direct dependency on internal/core/upstream — the
// upstream.Client's method signature matches this interface, so main.go
// can hand it in as-is.
type Prober interface {
	// FetchWallet is the cheapest per-account upstream call we have:
	// GET /workspaces/wallet returns credit balances and exercises the
	// full auth path (JWT mint, per-account HTTP client resolve, TLS
	// fingerprint) in one round trip.
	FetchWallet(ctx context.Context, account *domain.Account) (*ProbeWallet, error)
}

// ProbeWallet is the tiny shape the handler cares about — the upstream
// Wallet struct has more fields than the probe surfaces, but pulling
// the concrete type in would make this file transitively import
// internal/core/upstream. A local shape keeps the layering clean.
type ProbeWallet struct {
	WorkspaceID         string
	SubscriptionBalance int64
	CreditsBalance      int64
}

// ProbeResponse is the JSON body served by POST
// /admin/accounts/{id}/probe. Split into layer-specific booleans so an
// operator inspecting the WebUI can see at a glance which piece is
// failing.
type ProbeResponse struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	// LatencyMS is wall-clock time from handler entry to upstream
	// response — includes JWT mint + proxy dial + TLS handshake, so
	// this is the number to alert on when it grows.
	LatencyMS int64 `json:"latency_ms"`
	// Balance snapshot on success. Included so the operator can spot
	// "wallet is technically reachable but shows zero credits", which
	// is a common indicator of a subscription lapse.
	Balance *ProbeBalance `json:"balance,omitempty"`
	// Error carries the failure story on error. Structured so the
	// WebUI can style it (e.g. proxy timeouts differently from 401s).
	Error *ProbeError `json:"error,omitempty"`
	// TS lets an operator distinguish stale-cached UI states from
	// fresh probes.
	TS time.Time `json:"ts"`
}

type ProbeBalance struct {
	WorkspaceID  string `json:"workspace_id"`
	Subscription int64  `json:"subscription_hundredths"`
	Credits      int64  `json:"credits_hundredths"`
}

type ProbeError struct {
	// Kind is a coarse category so the UI can pick an icon / colour
	// without regex-matching the message. Values:
	//   "unauthorized"  — 401 from upstream, cookies / session dead
	//   "rate_limit"    — 429
	//   "forbidden"     — 403, usually DataDome or plan gate
	//   "upstream_5xx"  — 500+, Higgsfield is having a bad day
	//   "timeout"       — deadline exceeded (proxy / TLS / body)
	//   "network"       — dial failed, most often proxy misconfigured
	//   "internal"      — anything else, message carries the rest
	Kind string `json:"kind"`
	// Message is the human-readable string. Never contains cookies or
	// bearer tokens — the upstream layer already strips those.
	Message string `json:"message"`
}

// Probe implements POST /admin/accounts/{id}/probe. Not audited as a
// mutating operation because it does not change state; it also does
// not (yet) call the failover controller on failure — that decision
// stays with the pool's own error accounting on real traffic. This
// endpoint's promise is "give me an honest read of this account
// right now"; it deliberately does not sway the pool.
func (h *AccountsHandler) Probe(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "id is required")
		return
	}
	if h.Prober == nil {
		// Endpoint is always mounted so the frontend has a stable URL,
		// but a deployment that didn't wire an upstream client cannot
		// actually probe. 503 tells the WebUI to show a distinct
		// "probing is not configured on this server" state rather than
		// a generic error.
		writeErr(w, http.StatusServiceUnavailable, "probe_disabled",
			"probe endpoint not configured (no upstream client wired)")
		return
	}
	acc, err := h.Accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if acc.Status == domain.StatusBanned {
		// A banned account has had its credentials revoked (soft
		// delete). Probing it is guaranteed to fail and would waste an
		// upstream call, but more importantly we do not want to give
		// the operator a signal to "just unbanning would fix it" —
		// ban is a deliberate lifecycle state.
		writeErr(w, http.StatusConflict, "account_banned",
			"cannot probe a banned account")
		return
	}

	// Hard cap the probe at 15s so a wedged proxy / slow upstream
	// doesn't tie up an admin request. FetchWallet has its own
	// per-endpoint deadline via upstream.Client's timeouts map, but
	// adding an outer cap makes the WebUI behavior predictable.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	start := time.Now()
	wallet, err := h.Prober.FetchWallet(ctx, acc)
	elapsed := time.Since(start).Milliseconds()

	resp := ProbeResponse{
		AccountID: id,
		LatencyMS: elapsed,
		TS:        time.Now().UTC(),
	}
	if err != nil {
		resp.OK = false
		resp.Error = classifyProbeError(err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK) // probe failures are reported as body content, not HTTP errors
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	resp.OK = true
	resp.Balance = &ProbeBalance{
		WorkspaceID:  wallet.WorkspaceID,
		Subscription: wallet.SubscriptionBalance,
		Credits:      wallet.CreditsBalance,
	}
	writeJSON(w, http.StatusOK, resp)
}

// classifyProbeError buckets the upstream error into a coarse Kind so
// the WebUI can display it without string-matching. Matches the sentinel
// errors defined in domain/errors.go plus context.DeadlineExceeded and
// generic network failures.
func classifyProbeError(err error) *ProbeError {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, domain.ErrUpstreamTimeout):
		return &ProbeError{Kind: "timeout", Message: err.Error()}
	case errors.Is(err, domain.ErrUpstreamUnauthorized):
		return &ProbeError{Kind: "unauthorized", Message: err.Error()}
	case errors.Is(err, domain.ErrUpstreamForbidden):
		return &ProbeError{Kind: "forbidden", Message: err.Error()}
	case errors.Is(err, domain.ErrUpstreamRateLimit):
		return &ProbeError{Kind: "rate_limit", Message: err.Error()}
	case errors.Is(err, domain.ErrUpstreamServerError):
		return &ProbeError{Kind: "upstream_5xx", Message: err.Error()}
	default:
		// Network-layer errors from the utls client typically don't
		// wrap a domain sentinel — string-match a couple of common
		// substrings so the UI can still render an actionable label.
		msg := err.Error()
		if containsAny(msg, "connection refused", "no route to host",
			"dial tcp", "socks connect", "proxy error", "EOF") {
			return &ProbeError{Kind: "network", Message: msg}
		}
		return &ProbeError{Kind: "internal", Message: msg}
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if idx := indexOf(s, n); idx >= 0 {
			return true
		}
	}
	return false
}

// indexOf avoids pulling in strings just for one call in a hot-path-free
// helper.
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
