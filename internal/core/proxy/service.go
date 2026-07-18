// Package proxy is the reverse-proxy orchestration layer.
//
// It sits between the /v1 HTTP handlers and the upstream client, doing:
//
//   - alias resolution via ModelRegistry
//   - account pick via AccountStore.PickAndLock (respecting model gates)
//   - request-body construction (with SPA-default fills)
//   - job creation + terminal polling via core/upstream
//   - refund / balance bookkeeping on failure
//   - account unlock on completion
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/failover"
	"github.com/greensheep999/higgsgo/internal/core/metering"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/core/webhook"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Service is the reverse-proxy business object.
type Service struct {
	Store    ports.AccountStore
	Registry ports.ModelRegistry
	Upstream *upstream.Client
	Jobs     ports.JobStore // optional; when set every create is persisted
	Groups   ports.GroupStore // optional; when set, group RouteStrategy is resolved
	Logger   *slog.Logger
	Clock    ports.Clock

	// Failover, when non-nil, observes upstream outcomes and pulls
	// accounts out of rotation according to the [failover] policy.
	// Nil-safe: main.go leaves this unset when [failover].enabled is
	// false, and every method on the controller short-circuits on a
	// nil receiver so the proxy path stays a straight line.
	Failover *failover.Controller

	// APIKeys, when non-nil, is consulted at the terminal transition to look
	// up the caller's APIKey.MarkupPct so the Recorder can apply the
	// operator-configured markup on top of the actual upstream credits. If
	// the lookup fails or the key row is missing, we fall through with
	// markupPct=0 (Recorder treats that as 1.0, i.e. no markup) and log a
	// warning — metering must never block on this.
	APIKeys ports.APIKeyStore

	// Meter, when non-nil, receives a usage event after each terminal job.
	// The service only fires it on the sync path — the pollworker owns
	// metering for async jobs.
	Meter *metering.Recorder

	// Webhooks, when non-nil, delivers a signed terminal-state notification
	// to the caller-supplied callback URL after the sync poll finishes.
	// Fire is non-blocking; delivery + retries happen on background
	// goroutines owned by the Dispatcher. The async path is handled by the
	// pollworker (which owns the same Dispatcher instance).
	Webhooks *webhook.Dispatcher

	// AsyncByDefault, when true, makes async=true the default for video/
	// audio outputs even when the caller does not set the async field.
	// Images stay sync by default because they usually complete in <30s.
	AsyncByDefault bool

	// SyncPollDeadline caps how long sync requests block the HTTP caller.
	// Default 3m. Beyond this we return the job as queued and the client
	// polls GET /v1/jobs/{id} (backed by the pollworker).
	SyncPollDeadline time.Duration
}

// GenerationRequest is the normalized shape produced by the /v1 handler
// after parsing OpenAI-compatible input.
type GenerationRequest struct {
	// User-facing model alias, e.g. "seedance-2-0-mini".
	Model string

	// User-supplied params. Merged into the model's default body.
	// Values here take precedence over SPA defaults.
	UserParams map[string]any

	// Media inputs. When non-empty, the service injects them into params.medias[]
	// or params.input_image/input_video/input_audio according to the model spec.
	Media *MediaInput

	// Async, when true, returns as soon as the upstream create succeeds.
	// The caller polls /v1/jobs/{id} to fetch the result.
	Async bool

	// SyncRequested distinguishes "caller explicitly asked for sync" from
	// "caller did not specify". When false and AsyncByDefault is on, video
	// / audio outputs are treated as async even if Async is false.
	SyncRequested bool

	// PollDeadline caps synchronous poll time. Default 3m.
	PollDeadline time.Duration

	// GroupID for group-scoped pool pick. Empty selects the "default" group.
	GroupID string

	// APIKeyID / CPAPartnerID are set by the auth middleware for accounting.
	APIKeyID     string
	CPAPartnerID string

	// CallbackURL, when non-empty, is a caller-supplied HTTP endpoint that
	// receives a signed webhook once the job reaches a terminal state.
	// Persisted on the jobs row so the async pollworker path can fire it
	// even for requests that returned queued to the caller.
	CallbackURL string
}

// MediaInput represents a single reference media object provided by the caller.
type MediaInput struct {
	// PreUploadedID is the higgsfield media UUID; if set we use it directly.
	PreUploadedID string
	Type          string // "image" | "video" | "audio"
	URL           string
}

// GenerationResponse is what the /v1 handler returns to the API caller.
// When async or when a sync poll times out, PollURL is set so the caller
// knows where to look for the terminal state.
type GenerationResponse struct {
	ID         string              `json:"id"`
	Object     string              `json:"object"` // "video" | "image" | "audio"
	Model      string              `json:"model"`
	Status     string              `json:"status"` // "completed" | "queued" | "in_progress" | "failed"
	CreatedAt  int64               `json:"created_at"`
	ResultURL  string              `json:"result_url,omitempty"`
	UpstreamID string              `json:"upstream_job_id,omitempty"`
	Cost       int64               `json:"cost,omitempty"`
	Data       []map[string]string `json:"data,omitempty"` // OpenAI-shaped
	PollURL    string              `json:"poll_url,omitempty"`
	Error      *APIError           `json:"error,omitempty"`
}

// APIError is the error object returned to callers.
type APIError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

// Generate is the main entry point invoked by all three /v1 handlers.
//
// Flow:
//  1. resolve model
//  2. pick + lock an account (respecting gates)
//  3. call upstream.CreateJob (fast, ~1-3s)
//  4. persist a Job row (status=queued) so the pollworker takes over
//  5. either return immediately (async) or poll the DB row until terminal
func (s *Service) Generate(ctx context.Context, req GenerationRequest) (*GenerationResponse, error) {
	spec, err := s.Registry.Resolve(req.Model)
	if err != nil {
		return nil, fmt.Errorf("resolve model: %w", err)
	}

	// Decide async/sync. Async is the default for video/audio because those
	// models can take 30+ minutes upstream (wan_animate family).
	async := req.Async
	if !req.SyncRequested && s.AsyncByDefault && (spec.Output == "video" || spec.Output == "audio") {
		async = true
	}

	// Resolve the group policy once so route strategy, concurrency
	// caps, model regex, and budget stay consistent across the pick —
	// a second Get during PickAndLock could see a mid-request PUT of
	// the group config.
	policy := s.resolveGroupPolicy(ctx, req.GroupID)

	// Pre-pick group gates (ROADMAP P1-4): reject requests that would
	// pick an account only to fail at charge time. Runs before
	// PickAndLock so we don't consume an in_flight slot on a doomed
	// request. Nil-safe when Groups is unwired.
	if err := s.enforceGroupGates(policy, spec.Alias, spec.EstCostHundredths); err != nil {
		return nil, err
	}

	// Pick an account from the pool.
	pickParams := ports.PickParams{
		JST:                     spec.JST,
		EstCostHundredths:       spec.EstCostHundredths,
		RequiresPaid:            s.Registry.StarterLocked(spec.JST) || spec.RequiresPaid,
		RequiresUltra:           spec.RequiresUltra,
		GroupID:                 req.GroupID,
		RouteStrategy:           policy.RouteStrategy,
		MaxGroupInFlight:        policy.MaxGroupInFlight,
		MaxConcurrentPerAccount: policy.MaxConcurrentPerAccount,
	}
	acc, lockToken, err := s.Store.PickAndLock(ctx, pickParams)
	if err != nil {
		return nil, err
	}
	// Snapshot the pre-job balance so metering can attribute exact credits
	// consumed via (preBalance - post.SubscriptionBalance). Zero means
	// "unknown" downstream; the account was just picked so a real balance
	// should be present, but we guard against the row missing the field.
	preBalance := acc.SubscriptionBalance
	// Unlock is exactly-once and panic-safe: a sync.Once wraps the actual
	// Unlock call so early manual invocations (error path releases
	// before returning the mapped error; sync path releases after the
	// poll finishes) don't race with the top-level defer, which runs
	// even if any code between here and the return panics.
	//
	// Async handoff (ROADMAP P3-11): when the pollworker takes over
	// lifecycle for an async job, we set handedOff so the once-wrapped
	// decrement becomes a no-op. The pollworker itself calls
	// UpdateInFlight(-1) at every terminal transition (poll terminal,
	// timeout, or Failover-abort), so in_flight now reflects real
	// concurrent upstream load across BOTH sync and async paths — not
	// just the sync path as before. If the process crashes mid-async
	// the slot leaks, but ResetAllInFlight on boot (P0-2) cleans up on
	// next start.
	//
	// Before P0-2, three hand-rolled unlock() sites without a defer
	// meant a panic between PickAndLock and any of them (or an
	// unmapped early return added later) would leak the row's
	// in_flight_jobs counter forever.
	var unlockOnce sync.Once
	var handedOff atomic.Bool
	unlock := func() {
		unlockOnce.Do(func() {
			if handedOff.Load() {
				// Pollworker owns the release. Nothing to do here — it
				// will call UpdateInFlight(-1) at the terminal transition.
				return
			}
			// context.Background(): the caller's ctx may already be
			// cancelled by the time we release the slot; releasing must
			// always succeed regardless.
			if unlockErr := s.Store.Unlock(context.Background(), acc.ID, lockToken); unlockErr != nil && s.Logger != nil {
				s.Logger.Warn("pool unlock failed",
					slog.String("account_id", acc.ID),
					slog.String("err", unlockErr.Error()))
			}
		})
	}
	defer unlock()

	if s.Logger != nil {
		s.Logger.Info("picked account",
			slog.String("account_id", acc.ID),
			slog.String("plan", string(acc.PlanType)),
			slog.String("model", spec.Alias),
			slog.String("jst", spec.JST),
			slog.Bool("async", async),
			slog.Int64("est_cost_h", spec.EstCostHundredths))
	}

	// Build request body.
	body := buildBody(spec, req)

	created, err := s.Upstream.CreateJob(ctx, upstream.CreateRequest{
		Account:  acc,
		Endpoint: spec.Endpoint,
		Body:     body,
	})
	if err != nil {
		// Feed the failover controller before we let the error bubble
		// up. body-for-risk-marker is not readily available here (the
		// upstream client wraps the response body into a sentinel error
		// string), so we pass err.Error() to the throttle path so the
		// operator-configured RiskMarkers can still match on the
		// stringified body snippet the sentinel carries. Nil-safe.
		s.Failover.RecordError(ctx, acc.ID, err, err.Error())
		unlock()
		return nil, mapUpstreamError(err, spec)
	}

	// Persist to jobs table so the pollworker (and future /v1/jobs/{id})
	// can pick it up. Best-effort: if the store call fails we still return
	// the created job to the caller and log the error.
	requestTS := s.now()
	if s.Jobs != nil {
		job := &domain.Job{
			ID:              created.JobID,
			APIKeyID:        req.APIKeyID,
			CPAPartnerID:    req.CPAPartnerID,
			GroupID:         req.GroupID,
			AccountID:       acc.ID,
			ModelAlias:      spec.Alias,
			JST:             spec.JST,
			Endpoint:        spec.Endpoint,
			RequestBodyJSON: EnsureBodyJSON(body),
			RequestTS:       requestTS,
			UpstreamJobID:   created.JobID,
			UpstreamCost:    created.Cost,
			Status:          domain.JobQueued,
			// Snapshotted before create so the async pollworker can compute
			// exact credits consumed at terminal transition without having
			// to re-fetch the account row twice.
			PreBalanceH: preBalance,
			// Persisted so both the sync path (below, on the terminal
			// transition) and the async pollworker can fire a webhook to
			// the caller when the job finishes.
			CallbackURL: req.CallbackURL,
		}
		if err := s.Jobs.Create(ctx, job); err != nil && s.Logger != nil {
			s.Logger.Warn("persist job failed",
				slog.String("job_id", created.JobID),
				slog.String("err", err.Error()))
		}
	}

	if async {
		// Hand off in_flight ownership to the pollworker so the slot
		// stays reserved for the whole job lifetime — not just until
		// CreateJob returns. The deferred unlock() becomes a no-op and
		// the pollworker calls UpdateInFlight(-1) at every terminal
		// point (poll terminal, timeout, or fetch abort). See
		// docs/ROADMAP.md P3-11.
		handedOff.Store(true)
		return &GenerationResponse{
			ID:         created.JobID,
			Object:     objectForOutput(spec.Output),
			Model:      spec.Alias,
			Status:     "queued",
			CreatedAt:  requestTS.Unix(),
			UpstreamID: created.JobID,
			Cost:       created.Cost,
			PollURL:    fmt.Sprintf("/v1/jobs/%s", created.JobID),
		}, nil
	}

	// Sync path: poll upstream directly until terminal or deadline.
	// The pollworker will double-poll harmlessly if it wakes up during
	// this window — statuses are idempotent.
	deadline := req.PollDeadline
	if deadline == 0 {
		deadline = s.SyncPollDeadline
	}
	if deadline == 0 {
		deadline = 3 * time.Minute
	}
	final, err := s.Upstream.PollUntilTerminal(ctx, acc, created.JobID, upstream.PollOptions{
		Deadline: deadline,
		Interval: 4 * time.Second,
	})
	unlock()
	if err != nil {
		if errors.Is(err, domain.ErrUpstreamTimeout) {
			// Timeout is not account-attributable — the job may still
			// complete via the pollworker. Skip the failover feedback
			// and return queued.
			return &GenerationResponse{
				ID:         created.JobID,
				Object:     objectForOutput(spec.Output),
				Model:      spec.Alias,
				Status:     "queued",
				CreatedAt:  requestTS.Unix(),
				UpstreamID: created.JobID,
				Cost:       created.Cost,
				PollURL:    fmt.Sprintf("/v1/jobs/%s", created.JobID),
			}, nil
		}
		s.Failover.RecordError(ctx, acc.ID, err, err.Error())
		return nil, mapUpstreamError(err, spec)
	}
	// Also update the DB row so the pollworker skips this job on its next tick.
	if s.Jobs != nil {
		terminalStatus := domain.JobStatus(final.Status)
		if terminalStatus == "failed" && final.Refunded {
			terminalStatus = domain.JobRefunded
		} else if terminalStatus == "completed" {
			terminalStatus = domain.JobCompleted
		} else if terminalStatus == "failed" {
			terminalStatus = domain.JobFailed
		}
		_ = s.Jobs.UpdateStatus(ctx, created.JobID, terminalStatus, ports.JobMeta{
			ResultURL: final.ResultURL,
			LatencyMS: s.now().Sub(requestTS).Milliseconds(),
			Refunded:  final.Refunded,
		})
	}

	// Feed the failover controller with the terminal outcome. Only a
	// "completed" upstream status counts as a healthy run — a "failed"
	// / "nsfw" / "terminated" job is a content-level outcome (see the
	// design doc §8.1) and MUST NOT count against the account.
	// RecordSuccess is nil-safe.
	if final.Status == "completed" {
		s.Failover.RecordSuccess(ctx, acc.ID)
	}

	// Build the terminal Job snapshot once; it feeds both metering and the
	// webhook fire below. Kept outside the Meter branch so a deployment
	// running Webhooks-only (no Recorder wired) still notifies callers.
	terminalStatus := domain.JobStatus(final.Status)
	if terminalStatus == "failed" && final.Refunded {
		terminalStatus = domain.JobRefunded
	} else if terminalStatus == "completed" {
		terminalStatus = domain.JobCompleted
	} else if terminalStatus == "failed" {
		terminalStatus = domain.JobFailed
	}
	latency := s.now().Sub(requestTS).Milliseconds()
	mJob := &domain.Job{
		ID:            created.JobID,
		APIKeyID:      req.APIKeyID,
		CPAPartnerID:  req.CPAPartnerID,
		GroupID:       req.GroupID,
		AccountID:     acc.ID,
		ModelAlias:    spec.Alias,
		JST:           spec.JST,
		Endpoint:      spec.Endpoint,
		RequestTS:     requestTS,
		UpstreamJobID: created.JobID,
		UpstreamCost:  created.Cost,
		ResultURL:     final.ResultURL,
		Status:        terminalStatus,
		LatencyMS:     latency,
		Refunded:      final.Refunded,
		FinishedAt:    s.now(),
	}

	// Metering: emit a usage event now that we know the terminal outcome.
	// Fetch the account again to observe the post-job balance so we can
	// compute actual credits consumed. Best-effort — a missing account or
	// a metering failure must not block the API response.
	if s.Meter != nil {
		freshAcc, getErr := s.Store.Get(ctx, acc.ID)
		if getErr != nil || freshAcc == nil {
			// Fall back to the stale copy; Recorder will use upstream_cost
			// when it detects a zero or negative balance delta.
			freshAcc = acc
		}
		markup := s.resolveMarkup(ctx, req.APIKeyID)
		if err := s.Meter.OnJobTerminal(ctx, mJob, freshAcc, preBalance, markup); err != nil && s.Logger != nil {
			s.Logger.Warn("metering failed",
				slog.String("job_id", created.JobID),
				slog.String("err", err.Error()))
		}
	}

	// Webhook: fire-and-forget so the HTTP caller isn't blocked by delivery
	// latency or retries. Only fires when the caller supplied a callback URL
	// AND a Dispatcher is wired in — otherwise this branch is a no-op. The
	// pollworker owns the analogous fire on the async path.
	if s.Webhooks != nil && req.CallbackURL != "" {
		s.Webhooks.Fire(req.CallbackURL, mJob)
	}

	resp := &GenerationResponse{
		ID:         created.JobID,
		Object:     objectForOutput(spec.Output),
		Model:      spec.Alias,
		Status:     final.Status,
		CreatedAt:  requestTS.Unix(),
		UpstreamID: created.JobID,
		ResultURL:  final.ResultURL,
		Cost:       created.Cost,
	}
	if final.ResultURL != "" {
		resp.Data = []map[string]string{{"url": final.ResultURL}}
	}
	if final.Status == "failed" {
		resp.Error = &APIError{
			Type:    "upstream_fail",
			Message: "upstream generation failed",
		}
	}
	return resp, nil
}

// buildBody assembles the payload dispatched to higgsfield. We start from
// the example params encoded in the ModelSpec (future work — for now empty)
// and merge UserParams on top.
func buildBody(spec *domain.ModelSpec, req GenerationRequest) map[string]any {
	params := make(map[string]any)
	// Merge user-supplied params first.
	for k, v := range req.UserParams {
		params[k] = v
	}
	// Inject media if provided and the model wants it.
	if req.Media != nil && req.Media.PreUploadedID != "" {
		mediaObj := map[string]any{
			"id":   req.Media.PreUploadedID,
			"type": mediaTypeFor(req.Media.Type),
			"url":  req.Media.URL,
		}
		switch spec.MediaRole {
		case "medias":
			params["medias"] = []any{
				map[string]any{"role": "start_image", "data": mediaObj},
			}
		case "input_image", "":
			// Default: flat input_image when spec doesn't say otherwise.
			if _, ok := params["input_image"]; !ok {
				params["input_image"] = mediaObj
			}
		}
	}
	// Top-level fields (application_slug for nano_banana_2 family).
	top := map[string]any{
		"params":             params,
		"use_unlim":          false,
		"use_seedream_bonus": false,
	}
	if spec.ApplicationSlug != "" {
		top["application_slug"] = spec.ApplicationSlug
	}
	return top
}

func mediaTypeFor(kind string) string {
	switch kind {
	case "video":
		return "video_input"
	case "audio":
		return "audio_input"
	default:
		return "media_input"
	}
}

func objectForOutput(output string) string {
	switch output {
	case "video":
		return "video"
	case "audio":
		return "audio"
	default:
		return "image"
	}
}

// mapUpstreamError converts sentinel errors from core/upstream into the
// API-facing GenerationResponse.Error shape while preserving the raw error
// for logging.
func mapUpstreamError(err error, spec *domain.ModelSpec) error {
	// Currently we surface the error transparently. The /v1 handler
	// translates it into an HTTP status. This function stays as a hook
	// for future account-switching retries.
	_ = spec
	return err
}

// resolveMarkup looks up APIKey.MarkupPct for the given key id. Returns 0
// when APIKeys is nil, the key id is empty, or the lookup fails — the
// Recorder treats 0 as "no markup" (multiplier 1.0). A missing row after
// auth already verified the key is unlikely (revoked mid-flight) so we log
// at warn level and continue; accounting must not block on this.
func (s *Service) resolveMarkup(ctx context.Context, apiKeyID string) float64 {
	if s.APIKeys == nil || apiKeyID == "" {
		return 0
	}
	k, err := s.APIKeys.Get(ctx, apiKeyID)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("resolve markup failed",
				slog.String("api_key_id", apiKeyID),
				slog.String("err", err.Error()))
		}
		return 0
	}
	if k == nil {
		return 0
	}
	return k.MarkupPct
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// groupPolicy is the subset of Group config that PickAndLock consumes.
// Fetching it once in resolveGroupPolicy keeps route strategy and
// concurrency caps consistent for a single pick — a second Get during
// the tx could see a mid-request PUT of the group.
type groupPolicy struct {
	RouteStrategy           domain.RouteStrategy
	MaxGroupInFlight        int // 0 = uncapped
	MaxConcurrentPerAccount int // 0 = fall back to store default (5)

	// Model-alias gates and group-level monthly budget. Populated only
	// when Groups != nil and the group row exists. Enforced in
	// enforceGroupGates before the pick.
	//
	// AllowedModels / BlockedModels are pre-compiled once per pick so
	// PickAndLock does not re-parse the regex per candidate. A nil
	// pointer means the field was empty or invalid on the group row —
	// treated as "no filter" (allow), which matches the historical
	// behavior of silently ignoring these columns.
	AllowedModels       *regexp.Regexp
	BlockedModels       *regexp.Regexp
	MonthlyCreditBudget int64 // 0 = unbounded
	MonthlyCreditUsed   int64
}

// resolveGroupPolicy looks up the group's runtime policy. Returns zero
// values when no group store is wired, the group id is empty, or the
// group row is missing — semantically "no group constraints".
func (s *Service) resolveGroupPolicy(ctx context.Context, groupID string) groupPolicy {
	if s.Groups == nil || groupID == "" {
		return groupPolicy{RouteStrategy: domain.RouteRoundRobin}
	}
	g, err := s.Groups.Get(ctx, groupID)
	if err != nil || g == nil {
		return groupPolicy{RouteStrategy: domain.RouteRoundRobin}
	}
	strat := g.RouteStrategy
	if strat == "" {
		strat = domain.RouteRoundRobin
	}

	// Compile regexes tolerantly: an invalid pattern on the group row
	// falls back to "no filter" rather than 500-ing the caller. A WARN
	// log makes the misconfiguration visible.
	allowed := compileGroupRegex(g.AllowedModelsRegex, groupID, "allowed", s.Logger)
	blocked := compileGroupRegex(g.BlockedModelsRegex, groupID, "blocked", s.Logger)

	return groupPolicy{
		RouteStrategy:           strat,
		MaxGroupInFlight:        g.MaxConcurrentJobs,
		MaxConcurrentPerAccount: g.MaxConcurrentPerAccount,
		AllowedModels:           allowed,
		BlockedModels:           blocked,
		MonthlyCreditBudget:     g.MonthlyCreditBudget,
		MonthlyCreditUsed:       g.MonthlyCreditUsed,
	}
}

// enforceGroupGates runs the pre-pick eligibility checks that operate on
// the request + resolved group policy without needing an account row.
// Returns a domain error the caller can propagate straight to the HTTP
// layer, or nil to allow the request through to PickAndLock. Split out
// so unit tests can exercise the gate matrix without spinning up a
// full pool.
//
// Enforced today:
//   - blocked_models_regex: exact-match a compiled regex against the
//     canonical alias. Match → ErrModelBlocked.
//   - allowed_models_regex: if set, the alias must match. Miss →
//     ErrModelNotAllowed.
//   - monthly_credit_budget: est cost + already-used must stay under
//     the budget when the budget is set. Over → ErrGroupQuotaExhausted.
//
// Deferred (still stored-but-not-enforced): per-Key monthly_used gate,
// per-Key model regex. These require the resolver package's full
// KeyConfig cascade; the plumbing is ready but the caller (v1/handler)
// does not currently hand the APIKey to Generate.
func (s *Service) enforceGroupGates(policy groupPolicy, alias string, estCost int64) error {
	if policy.BlockedModels != nil && policy.BlockedModels.MatchString(alias) {
		return domain.ErrModelBlocked
	}
	if policy.AllowedModels != nil && !policy.AllowedModels.MatchString(alias) {
		return domain.ErrModelNotAllowed
	}
	if policy.MonthlyCreditBudget > 0 {
		// Reserve headroom: use est cost + already-charged. Fine to
		// approve when equal — the last credit-worth of the budget is
		// still spendable. Subsequent picks bump used, so budget is
		// self-limiting.
		if policy.MonthlyCreditUsed+estCost > policy.MonthlyCreditBudget {
			return domain.ErrGroupQuotaExhausted
		}
	}
	return nil
}

func compileGroupRegex(pattern, groupID, kind string, logger *slog.Logger) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		if logger != nil {
			logger.Warn("group regex invalid; treating as no filter",
				slog.String("group_id", groupID),
				slog.String("kind", kind),
				slog.String("pattern", pattern),
				slog.String("err", err.Error()))
		}
		return nil
	}
	return re
}

// EnsureJSON returns the body as a json.RawMessage for logging.
func EnsureJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// EnsureBodyJSON returns v as a compact JSON string, or "{}" on error.
// Used to persist request bodies in the jobs table.
func EnsureBodyJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
