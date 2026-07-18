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
