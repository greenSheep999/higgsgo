package failover

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// errorsIs is a local alias for errors.Is so the trivial helpers below
// don't need an "errors" import on top of everything else. Kept as a
// named function to make greps easier.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// resolvedConfig is the effective per-account tunable set after
// merging the global FailoverConfig with an optional per-account
// override row.
type resolvedConfig struct {
	Enabled bool

	ConsecEnabled   bool
	ConsecFailLimit int

	ThrottleEnabled bool
	JudgeWindowSec  int
	JudgeCount      int
	CooldownSec     int
	EvictWindowSec  int
	EvictCount      int

	OutageWindowSec  int
	OutageCountLimit int
}

// resolve loads the override row (if any) and merges it over the
// global config. On store errors we fall back to the global config
// alone — the controller should never fail-open on a transient DB
// hiccup during a decision path.
func (c *Controller) resolve(ctx context.Context, accountID string) resolvedConfig {
	base := resolvedConfig{
		Enabled: true,
	}
	if c != nil && c.Cfg != nil {
		base.Enabled = c.Cfg.Enabled
		base.ConsecEnabled = c.Cfg.Consecutive.Enabled
		base.ConsecFailLimit = c.Cfg.Consecutive.FailLimit
		base.ThrottleEnabled = c.Cfg.Throttle.Enabled
		base.JudgeWindowSec = c.Cfg.Throttle.JudgeWindowSec
		base.JudgeCount = c.Cfg.Throttle.JudgeCount
		base.CooldownSec = c.Cfg.Throttle.CooldownSec
		base.EvictWindowSec = c.Cfg.Throttle.EvictWindowSec
		base.EvictCount = c.Cfg.Throttle.EvictCount
		base.OutageWindowSec = c.Cfg.OutageGuard.WindowSec
		base.OutageCountLimit = c.Cfg.OutageGuard.DisableCountLimit
	}
	if base.ConsecFailLimit <= 0 && c != nil {
		base.ConsecFailLimit = c.FallbackFailLimit
	}
	if base.ConsecFailLimit <= 0 {
		base.ConsecFailLimit = 3
	}
	if base.JudgeWindowSec <= 0 {
		base.JudgeWindowSec = 60
	}
	if base.JudgeCount <= 0 {
		base.JudgeCount = 5
	}
	if base.CooldownSec <= 0 {
		base.CooldownSec = 600
	}
	if base.EvictWindowSec <= 0 {
		base.EvictWindowSec = 3600
	}
	if base.EvictCount <= 0 {
		base.EvictCount = 3
	}
	if base.OutageWindowSec <= 0 {
		base.OutageWindowSec = 30
	}
	if base.OutageCountLimit <= 0 {
		base.OutageCountLimit = 3
	}

	if c == nil || c.Overrides == nil || accountID == "" {
		return base
	}
	o, err := c.Overrides.Get(ctx, accountID)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("failover overrides lookup failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
		return base
	}
	if o == nil {
		return base
	}
	if o.Enabled != nil {
		base.Enabled = *o.Enabled
	}
	if o.FailLimit != nil && *o.FailLimit > 0 {
		base.ConsecFailLimit = *o.FailLimit
	}
	if o.JudgeWindowSec != nil && *o.JudgeWindowSec > 0 {
		base.JudgeWindowSec = *o.JudgeWindowSec
	}
	if o.JudgeCount != nil && *o.JudgeCount > 0 {
		base.JudgeCount = *o.JudgeCount
	}
	if o.CooldownSec != nil && *o.CooldownSec > 0 {
		base.CooldownSec = *o.CooldownSec
	}
	if o.EvictWindowSec != nil && *o.EvictWindowSec > 0 {
		base.EvictWindowSec = *o.EvictWindowSec
	}
	if o.EvictCount != nil && *o.EvictCount > 0 {
		base.EvictCount = *o.EvictCount
	}
	return base
}

// RecordSuccess clears the consecutive-fail streak on a successful
// upstream outcome. Nil-safe and store-error tolerant.
func (c *Controller) RecordSuccess(ctx context.Context, accountID string) {
	if c == nil || accountID == "" || c.Accounts == nil {
		return
	}
	if err := c.Accounts.ResetFailStreak(ctx, accountID); err != nil {
		if c.Logger != nil {
			c.Logger.Warn("failover reset streak failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
	}
}

// RecordFailure feeds mechanism ①. The httpStatus / detail arguments
// let callers who saw a raw response classify the failure without
// wrapping it in a sentinel; when they only have an err the wrapper
// RecordError below does that translation.
func (c *Controller) RecordFailure(ctx context.Context, accountID string, httpStatus int, detail string) {
	if c == nil || accountID == "" || c.Accounts == nil {
		return
	}
	cfg := c.resolve(ctx, accountID)
	if !cfg.Enabled || !cfg.ConsecEnabled {
		return
	}

	// Persist the event unconditionally — even when the outage guard
	// kicks in we still want the paper trail so operators can figure
	// out what happened after the fact.
	if c.Events != nil {
		reason := ReasonConsecFail
		if httpStatus == 401 {
			reason = ReasonAuthFailed
		} else if httpStatus == 0 {
			reason = ReasonNetwork
		}
		if err := c.Events.Insert(ctx, accountID, ports.FailoverEventFailure, reason, httpStatus); err != nil && c.Logger != nil {
			c.Logger.Warn("failover event insert failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
	}

	streak, err := c.Accounts.IncrFailStreak(ctx, accountID)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("failover incr streak failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
		return
	}
	if streak < cfg.ConsecFailLimit {
		return
	}

	// Streak reached the limit. Consult the pool-level outage guard
	// before actually flipping the row to disabled.
	if c.outageGuardTripped(ctx, cfg) {
		if c.Logger != nil {
			c.Logger.Warn("failover: outage guard tripped, skipping disable",
				slog.String("account_id", accountID),
				slog.Int("streak", streak),
				slog.Int("outage_window_sec", cfg.OutageWindowSec),
				slog.Int("outage_limit", cfg.OutageCountLimit))
		}
		if c.Notifier != nil {
			_ = c.Notifier.Send(ctx, ports.Notification{
				Level: ports.LevelWarn,
				Title: "higgsgo failover suppressed by outage guard",
				Body:  "consecutive-fail limit reached but the pool-level outage guard is tripped; the account was NOT disabled.",
				Tags: map[string]string{
					"account_id": accountID,
					"streak":     strconv.Itoa(streak),
					"guard":      "outage",
				},
			})
		}
		return
	}

	c.disable(ctx, accountID, ReasonConsecFail, detail, streak)
}

// RecordError is the sentinel-wrapper convenience form. Callers that
// have the wrapped domain error rather than a raw HTTP response should
// prefer this so the ClassIgnore path is applied uniformly. bodyForRisk
// is optional — pass "" if the caller doesn't have a body to sniff.
func (c *Controller) RecordError(ctx context.Context, accountID string, err error, bodyForRisk string) {
	if c == nil || accountID == "" || err == nil {
		return
	}
	class := Classify(err)
	switch class {
	case ClassIgnore:
		return
	case ClassAccountAttributable:
		c.RecordFailure(ctx, accountID, statusFromErr(err), err.Error())
	case ClassThrottle:
		c.RecordThrottle(ctx, accountID, bodyForRisk)
	}
}

// statusFromErr peels the well-known upstream sentinel back out of a
// wrapped error so callers who only have the wrapped form still populate
// account_failover_events.http_status with something useful. Returns 0
// for anything we don't recognise.
func statusFromErr(err error) int {
	switch {
	case err == nil:
		return 0
	case errorsIs(err, domain.ErrUpstreamUnauthorized):
		return 401
	case errorsIs(err, domain.ErrUpstreamRateLimit):
		return 429
	case errorsIs(err, domain.ErrUpstreamForbidden):
		return 403
	case errorsIs(err, domain.ErrUpstreamBadBody):
		return 422
	case errorsIs(err, domain.ErrUpstreamServerError):
		return 500
	}
	return 0
}

// RecordThrottle feeds mechanism ②. Callers pass the response body (or
// "") so the controller can decide whether the event counts as
// "generic throttle" or "risk_marker match" (which counts more heavily
// toward the eviction edge).
func (c *Controller) RecordThrottle(ctx context.Context, accountID string, body string) {
	if c == nil || accountID == "" || c.Accounts == nil {
		return
	}
	cfg := c.resolve(ctx, accountID)
	if !cfg.Enabled || !cfg.ThrottleEnabled {
		return
	}
	isRisk := c.isRiskMarker(body)

	// Record every throttle event; the judge / evict counts read this
	// same table so both edges see identical evidence.
	if c.Events != nil {
		reason := ReasonThrottle
		if isRisk {
			reason = ReasonRiskMarker
		}
		if err := c.Events.Insert(ctx, accountID, ports.FailoverEventThrottle, reason, 429); err != nil && c.Logger != nil {
			c.Logger.Warn("failover event insert failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
	}

	// Judge: count throttle events in the sliding window. When the
	// count crosses JudgeCount we cool the account down and also
	// record a "blacklist" event which feeds the eviction edge.
	throttleCount, err := c.countEvents(ctx, accountID, ports.FailoverEventThrottle, cfg.JudgeWindowSec)
	if err != nil && c.Logger != nil {
		c.Logger.Warn("failover judge count failed",
			slog.String("account_id", accountID),
			slog.String("err", err.Error()))
		return
	}
	if throttleCount < cfg.JudgeCount {
		return
	}

	// Cool down. Also stamp a blacklist event so the evict window can
	// see the transition.
	if c.Events != nil {
		if err := c.Events.Insert(ctx, accountID, ports.FailoverEventBlacklist, ReasonThrottle, 429); err != nil && c.Logger != nil {
			c.Logger.Warn("failover event insert (blacklist) failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
	}
	until := c.now().Add(time.Duration(cfg.CooldownSec) * time.Second)
	if err := c.Accounts.MarkThrottled(ctx, accountID, until, ReasonThrottle); err != nil {
		if c.Logger != nil {
			c.Logger.Warn("failover mark throttled failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
		return
	}
	if c.Logger != nil {
		c.Logger.Info("failover: account throttled",
			slog.String("account_id", accountID),
			slog.Time("until", until),
			slog.Int("count", throttleCount),
			slog.Int("window_sec", cfg.JudgeWindowSec))
	}

	// Escalation edge: N blacklist events in EvictWindowSec → disable.
	if c.Events == nil {
		return
	}
	blCount, err := c.countEvents(ctx, accountID, ports.FailoverEventBlacklist, cfg.EvictWindowSec)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("failover evict count failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
		return
	}
	if blCount < cfg.EvictCount {
		return
	}
	if c.outageGuardTripped(ctx, cfg) {
		if c.Logger != nil {
			c.Logger.Warn("failover: outage guard tripped, skipping evict-disable",
				slog.String("account_id", accountID),
				slog.Int("blacklist_count", blCount))
		}
		return
	}
	c.disable(ctx, accountID, ReasonEvict, "evict window exceeded", blCount)
}

// countEvents wraps Events.Count with a nil-safe error path.
func (c *Controller) countEvents(ctx context.Context, accountID string, kind ports.FailoverEventKind, windowSec int) (int, error) {
	if c.Events == nil {
		return 0, nil
	}
	return c.Events.Count(ctx, accountID, kind, windowSec)
}

// outageGuardTripped reports whether the pool-level circuit breaker
// wants us to skip a disable operation. Errors from the underlying
// count are treated as "guard NOT tripped" (fail-open on the controller
// side; we still write the event, we just don't disable) so a Store
// blip cannot silently freeze the failover response.
func (c *Controller) outageGuardTripped(ctx context.Context, cfg resolvedConfig) bool {
	if c == nil || c.Events == nil {
		return false
	}
	n, err := c.Events.CountRecentDisables(ctx, cfg.OutageWindowSec)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("failover outage guard count failed",
				slog.String("err", err.Error()))
		}
		return false
	}
	return n >= cfg.OutageCountLimit
}

// disable flips the account to StatusDisabled, records the "consec_fail"
// or "evict" event so the outage-guard count sees future disables, and
// fires an operator notification if a Notifier is wired. Called from
// both mechanisms on the terminal edge.
func (c *Controller) disable(ctx context.Context, accountID, reason, detail string, count int) {
	if c == nil || c.Accounts == nil {
		return
	}
	// Record the disable BEFORE flipping status so the outage-guard
	// counter can see this event; the row is what makes the guard
	// trip. This is intentional: if we disable first and then insert
	// the event, and two goroutines race, they can both slip under
	// the guard.
	if c.Events != nil {
		if err := c.Events.Insert(ctx, accountID, ports.FailoverEventFailure, reason, 0); err != nil && c.Logger != nil {
			c.Logger.Warn("failover event insert (disable) failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
	}
	if err := c.Accounts.MarkStatus(ctx, accountID, domain.StatusDisabled, reason); err != nil {
		if c.Logger != nil {
			c.Logger.Warn("failover disable failed",
				slog.String("account_id", accountID),
				slog.String("err", err.Error()))
		}
		return
	}
	if c.Logger != nil {
		c.Logger.Warn("failover: account disabled",
			slog.String("account_id", accountID),
			slog.String("reason", reason),
			slog.String("detail", detail),
			slog.Int("trigger_count", count))
	}
	if c.Notifier != nil {
		_ = c.Notifier.Send(ctx, ports.Notification{
			Level: ports.LevelError,
			Title: "higgsgo failover: account disabled",
			Body:  "The failover controller pulled an account out of rotation.",
			Tags: map[string]string{
				"account_id": accountID,
				"reason":     reason,
			},
		})
	}
}
