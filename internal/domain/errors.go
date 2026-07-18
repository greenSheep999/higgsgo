package domain

import "errors"

// Well-known domain errors surfaced to callers of core services.
// Adapters and HTTP handlers translate these into the wire format
// (HTTP status codes, OpenAI-shaped error objects).
var (
	ErrAccountNotFound     = errors.New("account not found")
	ErrNoEligibleAccount   = errors.New("no eligible account in pool")
	ErrAccountBusy         = errors.New("account concurrent limit reached")
	ErrAccountInsufficient = errors.New("account has insufficient balance")

	ErrGroupNotFound       = errors.New("group not found")
	ErrGroupQuotaExhausted = errors.New("group monthly budget exhausted")
	ErrGroupConcurrencyMax = errors.New("group concurrent job limit reached")

	ErrModelNotFound    = errors.New("model not found")
	ErrModelStarterOnly = errors.New("model requires a paid account tier")
	ErrModelUltraOnly   = errors.New("model requires ultra tier or higher")

	ErrAPIKeyNotFound    = errors.New("api key not found")
	ErrAPIKeyRevoked     = errors.New("api key revoked")
	ErrAPIKeyPaused      = errors.New("api key paused")
	ErrAPIKeyQuotaExceed = errors.New("api key monthly quota exhausted")

	ErrJobNotFound = errors.New("job not found")

	// ErrSettingNotFound is returned by SettingsStore.Get when the key
	// has no row. Callers that expect a fallback (e.g. TOML defaults)
	// should treat this as "no override" rather than propagating it.
	ErrSettingNotFound = errors.New("setting not found")

	// ErrModelOverrideNotFound — no override row for the alias. Treat
	// as "no override, spec defaults apply", don't propagate as error.
	ErrModelOverrideNotFound = errors.New("model override not found")

	// Registrar (higgsfield account registration flow). The slim build
	// ships a stub Registrar returning ErrRegistrarDisabled so admin
	// handlers answer 503 with a stable error shape. Real puppeteer /
	// OTP / captcha implementation is compiled in behind build tag
	// "register".
	ErrRegistrarDisabled    = errors.New("registrar disabled (build without 'register' tag)")
	ErrRegistrationNotFound = errors.New("registration not found")

	ErrUpstreamTimeout      = errors.New("upstream did not reach terminal state within deadline")
	ErrUpstreamRateLimit    = errors.New("upstream rate limit reached")
	ErrUpstreamUnauthorized = errors.New("upstream 401: JWT invalid or session expired")
	ErrUpstreamForbidden    = errors.New("upstream 403: plan gate")
	ErrUpstreamBadBody      = errors.New("upstream 422: body validation failed")
	ErrUpstreamServerError  = errors.New("upstream 5xx")
)
