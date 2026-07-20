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
	// ErrModelNotAllowed signals the caller's group has an allowed
	// models regex that does not match the requested alias. Distinct
	// from ErrModelNotFound (unknown alias) and ErrModelBlocked
	// (explicit deny) so the HTTP layer can render a precise message.
	ErrModelNotAllowed = errors.New("model not in group's allowed list")
	// ErrModelBlocked signals the caller's group has a blocked_models
	// regex that matches the requested alias. Enforced before pick so
	// the account balance is not touched.
	ErrModelBlocked = errors.New("model blocked by group policy")

	ErrAPIKeyNotFound    = errors.New("api key not found")
	ErrAPIKeyRevoked     = errors.New("api key revoked")
	ErrAPIKeyPaused      = errors.New("api key paused")
	ErrAPIKeyQuotaExceed = errors.New("api key monthly quota exhausted")

	ErrJobNotFound = errors.New("job not found")

	// ErrUsageEventDuplicate is returned by UsageEventStore.Insert when a
	// row with the same higgsgo_job_id already exists. Callers should
	// treat this as "another observer already recorded this terminal
	// transition — skip retrying"; it is a race outcome from the F1 fix
	// (concurrent sync + pollworker terminals) and not a corruption
	// signal. Metering.Recorder distinguishes it from other insert
	// failures so the log level stays at debug.
	ErrUsageEventDuplicate = errors.New("usage event already recorded for this job")

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
