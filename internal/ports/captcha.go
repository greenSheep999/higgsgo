package ports

import "context"

// CaptchaSolver defers CAPTCHA challenges to a third-party API (CapSolver,
// 2Captcha, NopeCHA, AntiCaptcha) or a human (manual adapter).
//
// higgsfield.ai puts DataDome in front of every page, so SolveDataDome is
// the hot path. Generic Solve is reserved for future needs (Turnstile,
// hCaptcha, reCAPTCHA v2/v3).
type CaptchaSolver interface {
	// SolveDataDome exchanges a captcha challenge for a valid `datadome`
	// cookie value and the corresponding x-datadome-clientid.
	// The solver must use req.ProxyURL to keep the resulting cookie
	// IP-bound to the caller.
	SolveDataDome(ctx context.Context, req DataDomeReq) (DataDomeResult, error)

	// Solve is a generic CAPTCHA entry point.
	Solve(ctx context.Context, req CaptchaReq) (CaptchaResult, error)

	// Balance returns the remaining prepaid balance in USD (or the
	// provider's native unit if USD is not available).
	Balance(ctx context.Context) (float64, error)

	Name() string
}

// DataDomeReq is a single DataDome challenge to solve.
type DataDomeReq struct {
	SiteURL    string
	CaptchaURL string
	UserAgent  string
	ProxyURL   string // same egress IP as the caller
}

// DataDomeResult is the outcome of a solved DataDome challenge.
type DataDomeResult struct {
	Cookie   string // value of the `datadome` cookie
	ClientID string // value of the x-datadome-clientid header
}

// CaptchaType lists the challenge families we may need to solve.
type CaptchaType string

const (
	CaptchaTurnstile CaptchaType = "turnstile"
	CaptchaHCaptcha  CaptchaType = "hcaptcha"
	CaptchaRecaptV2  CaptchaType = "recaptcha_v2"
	CaptchaRecaptV3  CaptchaType = "recaptcha_v3"
)

// CaptchaReq is a generic CAPTCHA challenge.
type CaptchaReq struct {
	Type      CaptchaType
	SiteKey   string
	SiteURL   string
	UserAgent string
	ProxyURL  string
	Metadata  map[string]string
}

// CaptchaResult carries the solved token.
type CaptchaResult struct {
	Token    string
	Metadata map[string]string
}
