package register

import (
	"context"
	"time"
)

type BrowserAutomator interface {
	Launch(ctx context.Context, opts LaunchOpts) (BrowserSession, error)
	Name() string
}

type BrowserSession interface {
	Goto(ctx context.Context, url string) error
	Fill(ctx context.Context, selector, value string) error
	Click(ctx context.Context, selector string) error
	WaitFor(ctx context.Context, selector string, timeout time.Duration) error
	Cookies(ctx context.Context) ([]Cookie, error)
	LocalStorage(ctx context.Context, key string) (string, error)
	UserAgent(ctx context.Context) (string, error)
	EvalJS(ctx context.Context, expr string) (string, error)
	Close() error
}

type MailboxProvider interface {
	FetchOTP(ctx context.Context, email string, since time.Time) (OTPResult, error)
	Supports(domain string) bool
}

type CaptchaSolver interface {
	SolveDataDome(ctx context.Context, pageURL, captchaURL string) (string, error)
	Solve(ctx context.Context, siteKey, pageURL, captchaType string) (string, error)
}

type RegistrationStore interface {
	Enqueue(ctx context.Context, req EnqueueRequest) (string, error)
	NextPending(ctx context.Context) (*Registration, error)
	MarkRunning(ctx context.Context, id string) error
	MarkOTPWait(ctx context.Context, id string) error
	MarkCompleted(ctx context.Context, id string, result CompletedResult) error
	MarkFailed(ctx context.Context, id string, reason string) error
	Get(ctx context.Context, id string) (*Registration, error)
	List(ctx context.Context, filter ListFilter) ([]Registration, error)
	Retry(ctx context.Context, id string) error
}

// Driver is the higher-level alternative to BrowserAutomator: instead
// of running the flow step-by-step in Go (Launch → Goto → Fill →
// Click …), a Driver runs the whole registration server-side and
// returns the harvested result. This matches the shape of the
// existing Node-side higgsfield-register project — its flow is a
// black-box `registerAccount(opts)` call that either produces
// cookies + UA + session token, or an error. See ROADMAP §5.4 P4-3b.
//
// When Flow is constructed with a non-nil Driver, Flow.Execute
// delegates to Driver.Register instead of walking the session-level
// path. The mailbox / captcha / browser adapters are still used by
// the driver internally — Flow just doesn't orchestrate them here.
//
// Two implementations planned:
//   - camoufox/driver_node.go — spawns a Node subprocess wrapping
//     the higgsfield-register project's registerAccount() call and
//     talks to it over HTTP. Production-facing (ROADMAP §5.4).
//   - mock/driver.go        — an in-process fake used to prove the
//     Flow-driver plumbing works end-to-end without touching
//     Higgsfield.
type Driver interface {
	// Register runs one signup end-to-end. Returns the harvested
	// credentials on success. Errors surface verbatim to the caller
	// (Flow.Execute → registration_store.MarkFailed) — no per-error
	// translation happens inside the driver.
	Register(ctx context.Context, req RegisterRequest) (CompletedResult, error)
	// Name is a short identifier for logs / metrics
	// ("camoufox-node", "mock", …).
	Name() string
}

// RegisterRequest is the input to Driver.Register — the subset of a
// pending Registration that the driver actually needs. Keeps the
// interface stable if the store row grows more admin-only columns.
type RegisterRequest struct {
	Email       string
	Password    string
	OAuthSource string
	ProxyURL    string
}
