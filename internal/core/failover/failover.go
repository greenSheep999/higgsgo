// Package failover implements the automatic account isolation subsystem.
//
// The controller observes account-attributable upstream failures and
// decides when to pull an account out of rotation. Two independent
// mechanisms coexist:
//
//   - Mechanism ①: consecutive-fail eviction. After FailLimit account-
//     attributable failures in a row, the account is disabled. This is
//     the MVP and on by default.
//
//   - Mechanism ②: sliding-window throttle judging. Independent of ①;
//     off by default. A rolling window over higgsfield 429 / anti-bot
//     signals cools accounts down (status=throttled + throttled_until
//     = now + CooldownSec). Repeat trips inside an EvictWindow escalate
//     the account to disabled.
//
// A pool-level circuit breaker sits over both mechanisms: when many
// accounts get disabled in a short window, we assume higgsfield itself
// is degraded and stop disabling more accounts so the pool doesn't
// drain during a global incident.
//
// The controller is intentionally simple: nothing here should throw or
// panic on a nil dependency. Callers (proxy service, pollworker) can
// safely hold a nil *Controller and every method becomes a no-op.
package failover

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// reasonEnabled is the sub-set of reason opcodes the controller writes
// to accounts.status_reason and account_failover_events.reason. Kept
// centralised so the admin surface can render them consistently.
const (
	ReasonConsecFail      = "consec_fail"
	ReasonEvict           = "evict"
	ReasonThrottle        = "throttle"
	ReasonRiskMarker      = "risk_marker"
	ReasonAuthFailed      = "auth_failed"
	ReasonNetwork         = "network"
	ReasonPoolOutageGuard = "outage_guard"
	ReasonManualRecover   = "manual_recover"
)

// Classification is the controller's internal verdict on an upstream
// error. See Classify for the mapping.
type Classification int

const (
	// ClassIgnore — the error is not account-attributable (5xx / 422 /
	// 403 / plain timeout). The controller does nothing.
	ClassIgnore Classification = iota
	// ClassThrottle — the error is a rate-limit / anti-bot signal. Feeds
	// mechanism ②'s judge window.
	ClassThrottle
	// ClassAccountAttributable — the error implicates the account
	// itself (401 after retry, network reset, DataDome challenge). Feeds
	// mechanism ①'s fail streak.
	ClassAccountAttributable
)

// Controller is the failover decision engine. Nil-safe: every method
// short-circuits when the receiver is nil so callers can hold an
// optional pointer.
type Controller struct {
	Accounts  ports.AccountStore
	Events    ports.FailoverEventStore
	Overrides ports.FailoverOverridesStore
	Notifier  ports.Notifier
	Logger    *slog.Logger
	Clock     func() time.Time

	// Cfg is a copy of the failover section. Read directly for each
	// decision; the admin config-update endpoint mutates it in place
	// (with a mutex on the enclosing Service).
	Cfg *config.FailoverConfig

	// FallbackFailLimit is the legacy [pool].fail_streak_threshold
	// value, used when Cfg.Consecutive.FailLimit is 0. Kept as a
	// separate field so a fresh deployment can just set the new
	// setting and never look at the deprecated one.
	FallbackFailLimit int
}

// New builds a Controller with sensible zero-value guards. Callers
// still need to populate Accounts + Events for it to do anything
// useful. Nil-safe when returned; a fully-empty Controller behaves
// exactly like a nil pointer.
func New(cfg *config.FailoverConfig, accounts ports.AccountStore, events ports.FailoverEventStore, overrides ports.FailoverOverridesStore, notifier ports.Notifier, logger *slog.Logger) *Controller {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	return &Controller{
		Accounts:  accounts,
		Events:    events,
		Overrides: overrides,
		Notifier:  notifier,
		Logger:    logger,
		Cfg:       cfg,
	}
}

// now returns the current time via the injected Clock (if any) or
// time.Now otherwise. Kept small so tests can drive a deterministic
// clock through.
func (c *Controller) now() time.Time {
	if c == nil {
		return time.Now()
	}
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now()
}

// Classify inspects err and reports which mechanism the failover
// controller should feed. Callers that don't have a classified error
// (e.g., they saw a raw HTTP status) can also call ClassifyStatus.
func Classify(err error) Classification {
	if err == nil {
		return ClassIgnore
	}
	// Domain sentinels take precedence over generic wrappers.
	switch {
	case errors.Is(err, domain.ErrUpstreamRateLimit):
		return ClassThrottle
	case errors.Is(err, domain.ErrUpstreamUnauthorized):
		// The upstream client's doWithRetry already burnt one 401
		// retry with a fresh JWT before surfacing this sentinel, so
		// the second failure is treated as "the account itself is
		// dead" (banned session, revoked clerk key).
		return ClassAccountAttributable
	case errors.Is(err, domain.ErrUpstreamForbidden),
		errors.Is(err, domain.ErrUpstreamBadBody),
		errors.Is(err, domain.ErrUpstreamServerError),
		errors.Is(err, domain.ErrUpstreamTimeout),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled):
		return ClassIgnore
	}

	// Network errors — the account is not necessarily bad (it could be
	// upstream flakiness), but the pool router should back off from
	// this row for a bit. We funnel these into mechanism ①.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ClassAccountAttributable
	}
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return ClassAccountAttributable
	}

	// TODO(failover): once the upstream client exposes an
	// ErrDataDomeChallenge sentinel (currently DataDome hits surface
	// as an opaque HTTP error), route it to ClassAccountAttributable
	// here. Grep the error body for the marker in the meantime.
	if s := err.Error(); s != "" {
		lower := strings.ToLower(s)
		if strings.Contains(lower, "datadome") || strings.Contains(lower, "captcha") {
			return ClassAccountAttributable
		}
	}
	return ClassIgnore
}

// ClassifyStatus maps an HTTP status code + body snippet to a class.
// Used by callers that have the raw upstream response but no wrapped
// sentinel error.
func ClassifyStatus(status int, body string) Classification {
	switch {
	case status == 401:
		return ClassAccountAttributable
	case status == 429:
		return ClassThrottle
	case status == 403, status == 422:
		return ClassIgnore
	case status >= 500:
		return ClassIgnore
	}
	// Generic body sniff for DataDome / captcha hints on non-standard
	// statuses (e.g., a 200 that redirects into a DataDome challenge).
	if body != "" {
		lower := strings.ToLower(body)
		if strings.Contains(lower, "datadome") || strings.Contains(lower, "captcha") {
			return ClassAccountAttributable
		}
	}
	return ClassIgnore
}

// isRiskMarker reports whether the response body carries a substring
// from the operator-configured RiskMarkers list. Empty list means
// "every throttle counts equally" — see the config comment.
func (c *Controller) isRiskMarker(body string) bool {
	if c == nil || c.Cfg == nil || body == "" {
		return false
	}
	markers := c.Cfg.Throttle.RiskMarkers
	if len(markers) == 0 {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range markers {
		if m == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
