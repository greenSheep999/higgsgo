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
	"github.com/greensheep999/higgsgo/internal/util/idgen"
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

	// MaxInFlightPerAccountDefault is the deployment-wide fallback cap on
	// concurrent jobs per account. Applied via PickParams when neither
	// accounts.max_concurrent (per-row) nor group.max_concurrent_per_account
	// (per-group) yield a tighter value. Zero preserves the store's
	// historical hardcoded fallback of 5, so a deployment that leaves the
	// TOML default untouched behaves identically to pre-F4.
	//
	// Precedence (tightest wins): accounts.max_concurrent (F4) &
	// group.max_concurrent_per_account & this field & 5.
	MaxInFlightPerAccountDefault int
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

	// GroupID for group-scoped pool pick. Empty selects the "default"
	// (global) pool. Also carried on the persisted Job row for
	// accounting — for spillover requests this is the group that
	// ACTUALLY picked, not necessarily the first candidate.
	GroupID string

	// GroupCandidates is the ordered spillover list from the v1
	// handler (ROADMAP P3-10). When a key is bound to multiple
	// groups the handler builds this list sorted by group name;
	// Generate tries each in order and falls over on
	// ErrGroupConcurrencyMax / ErrGroupQuotaExhausted /
	// ErrNoEligibleAccount. Errors that are not group-scoped
	// (ErrModelBlocked, ErrAPIKeyQuotaExceed, etc.) short-circuit
	// the whole request — no point trying another group if the
	// alias is blocked policy-wide or the caller's own key ran out.
	//
	// Empty or single-element slice = no spillover; behavior matches
	// pre-P3-10 code.
	GroupCandidates []string

	// APIKeyID / CPAPartnerID are set by the auth middleware for accounting.
	APIKeyID     string
	CPAPartnerID string

	// APIKeyMonthlyQuota / APIKeyMonthlyUsed carry the caller key's
	// quota state so enforceKeyGates can reject a request that would
	// exceed the monthly limit BEFORE the pool pick spends the slot.
	// Zero quota means unlimited (the historical default; the
	// recorder-time check in metering.Recorder still catches drift
	// against actual charged credits, but pre-pick rejection is the
	// only place a 402 quota_exhausted can be returned before the
	// job actually runs). See docs/ROADMAP.md P2-9.
	//
	// Populated by the /v1 handlers from
	// middleware.APIKeyFromContext(); left zero for internal / CPA
	// paths that carry their own accounting.
	APIKeyMonthlyQuota int64
	APIKeyMonthlyUsed  int64

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

	// Pre-pick per-Key gates (ROADMAP P2-9): reject a request whose
	// caller key has already exhausted its monthly quota. Runs BEFORE
	// spillover — key limits are not group-scoped, so an over-quota
	// request should fail fast regardless of which group we would try
	// next. Nil-safe: zero quota (== unlimited) is a no-op.
	if err := enforceKeyGates(req, spec.EstCostHundredths); err != nil {
		return nil, err
	}

	// Cross-group spillover (ROADMAP P3-10): try each candidate in
	// order. When the caller pinned a single group (or the key has
	// only one binding) the loop degenerates to one iteration.
	// Failover triggers on group-scoped capacity errors only —
	// ErrGroupConcurrencyMax / ErrGroupQuotaExhausted /
	// ErrNoEligibleAccount — because those may succeed against a
	// less-loaded sibling group. ErrModelBlocked and
	// ErrModelNotAllowed are policy statements that should not vary
	// by group (matching aliases in one group's regex but not
	// another's is a legitimate use case, so those DO fall through
	// to the next group). The final error is whatever the LAST
	// attempt returned so the caller sees an accurate reason.
	candidates := req.GroupCandidates
	if len(candidates) == 0 {
		// Legacy callers that don't set GroupCandidates (CPA plugin,
		// tests) get a one-shot with the pinned GroupID.
		candidates = []string{req.GroupID}
	}
	var (
		acc       *domain.Account
		lockToken string
		pickedID  string
		pickErr   error
	)
	for i, gid := range candidates {
		p := s.resolveGroupPolicy(ctx, gid)
		// Pre-pick group gates (P1-4): reject on model-regex OR
		// budget miss — but only for this candidate. A blocked
		// alias in group A may still be allowed in group B, so
		// treat these as spillover-eligible.
		if gateErr := s.enforceGroupGates(p, spec.Alias, spec.EstCostHundredths); gateErr != nil {
			pickErr = gateErr
			if i < len(candidates)-1 && isSpilloverEligible(gateErr) {
				continue
			}
			return nil, gateErr
		}
		// Per-account cap precedence: prefer the group value, then fall
		// back to the deployment default so a group that leaves
		// MaxConcurrentPerAccount at zero (the common case) still
		// respects the operator's TOML setting. Zero from both sources
		// lets the store apply its hardcoded fallback (5).
		perAcctCap := p.MaxConcurrentPerAccount
		if perAcctCap == 0 {
			perAcctCap = s.MaxInFlightPerAccountDefault
		}
		pickParams := ports.PickParams{
			JST:                     spec.JST,
			EstCostHundredths:       spec.EstCostHundredths,
			RequiresPaid:            s.Registry.StarterLocked(spec.JST) || spec.RequiresPaid,
			RequiresUltra:           spec.RequiresUltra,
			GroupID:                 gid,
			RouteStrategy:           p.RouteStrategy,
			MaxGroupInFlight:        p.MaxGroupInFlight,
			MaxConcurrentPerAccount: perAcctCap,
		}
		var err error
		acc, lockToken, err = s.Store.PickAndLock(ctx, pickParams)
		if err != nil {
			pickErr = err
			if i < len(candidates)-1 && isSpilloverEligible(err) {
				if s.Logger != nil {
					s.Logger.Info("group spillover",
						slog.String("from_group", gid),
						slog.String("err", err.Error()))
				}
				continue
			}
			return nil, err
		}
		// Success — record which group actually served the request.
		// The winning policy is not currently used after pick, but
		// the loop keeps it around under `p` so a future consumer
		// (e.g. per-group markup) can read from a stable var.
		_ = p
		pickedID = gid
		pickErr = nil
		break
	}
	if acc == nil {
		// Every candidate failed; propagate the last error.
		if pickErr == nil {
			pickErr = domain.ErrNoEligibleAccount
		}
		return nil, pickErr
	}
	// Rewrite req.GroupID to the group that ACTUALLY served the pick
	// so downstream accounting (Job row, metering event, webhook,
	// pollworker) is honest about which group the credits landed on.
	// The original candidate list is retained by value in req.
	req.GroupID = pickedID
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
		// Record a failed usage_event even though upstream never returned
		// a job id. Without this, sync-path CreateJob failures (401 stale
		// session, 422 body error, 429 rate limit, 5xx) are completely
		// invisible in the dashboard — the operator only sees the pool
		// getting hammered via access logs. HiggsgoJobID is minted locally
		// with a distinct "cf" prefix so audit tooling can spot rows that
		// never had an upstream counterpart. Runs before unlock() so a
		// meter store failure does not block the slot release below.
		if s.Meter != nil {
			mJob := &domain.Job{
				ID:           idgen.NewID("cf"),
				APIKeyID:     req.APIKeyID,
				CPAPartnerID: req.CPAPartnerID,
				GroupID:      req.GroupID,
				AccountID:    acc.ID,
				ModelAlias:   spec.Alias,
				JST:          spec.JST,
				Endpoint:     spec.Endpoint,
				RequestTS:    s.now(),
				UpstreamCost: 0,
				Status:       domain.JobFailed,
				ErrorType:    classifyCreateErr(err),
				LatencyMS:    0,
				FinishedAt:   s.now(),
			}
			markup := s.resolveMarkup(ctx, req.APIKeyID)
			if merr := s.Meter.OnJobTerminal(ctx, mJob, acc, 0, markup); merr != nil && s.Logger != nil {
				s.Logger.Warn("metering create-failed job",
					slog.String("job_id", mJob.ID),
					slog.String("err", merr.Error()))
			}
		}
		unlock()
		return nil, mapUpstreamError(err, spec)
	}

	// Persist to jobs table so the pollworker (and future /v1/jobs/{id})
	// can pick it up. Track whether the row actually landed — a Create
	// failure changes the terminal-handling semantics below: without a
	// jobs row, TryMarkTerminal cannot CAS (there's nothing to update)
	// and the pollworker's ListPending never sees the job either. In
	// that case the sync path is the ONLY observer and must run
	// metering + webhook + unlock unconditionally; async handoff is
	// unsafe and we degrade to sync polling.
	requestTS := s.now()
	jobPersisted := false
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
		if err := s.Jobs.Create(ctx, job); err != nil {
			if s.Logger != nil {
				s.Logger.Warn("persist job failed",
					slog.String("job_id", created.JobID),
					slog.String("err", err.Error()))
			}
		} else {
			jobPersisted = true
		}
	}

	if async && jobPersisted {
		// Hand off in_flight ownership to the pollworker so the slot
		// stays reserved for the whole job lifetime — not just until
		// CreateJob returns. The deferred unlock() becomes a no-op and
		// the pollworker calls UpdateInFlight(-1) at every terminal
		// point (poll terminal, timeout, or fetch abort). See
		// docs/ROADMAP.md P3-11.
		//
		// Requires jobPersisted: if Jobs.Create failed the row does
		// not exist, ListPending will never surface it, and the
		// in-flight slot would leak. In that case we fall through to
		// the sync-poll path below so this request completes on its
		// own credentials.
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
	if async && !jobPersisted && s.Logger != nil {
		// Async was requested but persistence failed — degrade to sync
		// so the caller still gets a real terminal outcome instead of
		// a queued response that no pollworker will ever pick up.
		s.Logger.Warn("async requested but jobs row missing; degrading to sync poll",
			slog.String("job_id", created.JobID))
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
	// unlock() release is deferred past the CAS below (line ~610) so the
	// slot decrement is gated on winning the race — otherwise the
	// concurrent pollworker's releaseInFlight would double-decrement and
	// prematurely free a slot (MAX(0,...) prevents underflow, but the
	// count still drifts under load).
	if err != nil {
		unlock() // Error path: no CAS ran, release now.
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
	// Feed the failover controller with the terminal outcome. Only a
	// "completed" upstream status counts as a healthy run — a "failed"
	// / "nsfw" / "terminated" job is a content-level outcome (see the
	// design doc §8.1) and MUST NOT count against the account.
	// RecordSuccess is nil-safe. Runs independently of the CAS below
	// because failover feedback is idempotent and the winner may be the
	// pollworker rather than this sync call.
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

	// F1 compare-and-swap: only one observer of the terminal transition
	// (this sync path OR the background pollworker) may run metering /
	// webhook. TryMarkTerminal atomically moves the jobs row into
	// terminalStatus iff it is still queued / pending / in_progress; the
	// won flag tells us whether we or the pollworker performed the write.
	//
	// Three cases matter for the side-effect gate below:
	//
	//   (a) jobPersisted && CAS won  → we own the transition. Run
	//       meter+webhook, release the slot.
	//   (b) jobPersisted && CAS lost with err == nil → pollworker (or
	//       another observer) already terminated the row and ran its
	//       own meter+webhook+release. Skip ours to avoid double-billing.
	//   (c) !jobPersisted OR CAS returned err != nil → we cannot prove
	//       another observer ran the side effects. jobs row is missing
	//       (Create failed) or the CAS itself errored (DB flakiness).
	//       In both sub-cases the sync path is the only guaranteed
	//       observer; treat this as a WIN and run meter+webhook+release
	//       ourselves. The migration-018 UNIQUE index on
	//       usage_events(higgsgo_job_id) is the belt-and-braces guard
	//       against the unlikely case that pollworker ALSO succeeds
	//       — a duplicate insert there surfaces ErrUsageEventDuplicate
	//       which the metering layer swallows.
	won := true
	if jobPersisted && s.Jobs != nil {
		var casErr error
		won, casErr = s.Jobs.TryMarkTerminal(ctx, created.JobID,
			[]domain.JobStatus{domain.JobQueued, domain.JobPending, domain.JobRunning},
			terminalStatus,
			ports.JobMeta{
				ResultURL: final.ResultURL,
				LatencyMS: latency,
				Refunded:  final.Refunded,
			})
		if casErr != nil {
			// Treat CAS DB errors as WIN — the sync path is guaranteed
			// to have observed the terminal, and we would rather
			// double-write (blocked by the UNIQUE index) than silently
			// drop the accounting row entirely.
			if s.Logger != nil {
				s.Logger.Warn("terminal CAS failed — treating as winner for side-effects",
					slog.String("job_id", created.JobID),
					slog.String("err", casErr.Error()))
			}
			won = true
		} else if !won && s.Logger != nil {
			s.Logger.Info("terminal CAS lost race — pollworker or another observer already terminated",
				slog.String("job_id", created.JobID),
				slog.String("status", string(terminalStatus)))
		}
	}

	// In-flight slot release: gated on `won` so we don't double-decrement
	// the account's in_flight_jobs counter. When the pollworker wins the
	// CAS it already called releaseInFlight; MAX(0,...) prevents actual
	// underflow, but a second decrement would still free an extra slot
	// prematurely and let the pool over-commit that account. Setting
	// handedOff before unlock() makes the unlockOnce a no-op — the winner
	// (pollworker) owns the release in the losing branch.
	if !won {
		handedOff.Store(true)
	}
	unlock()

	// Metering: emit a usage event now that we know the terminal outcome.
	// Fetch the account again to observe the post-job balance so we can
	// compute actual credits consumed. Best-effort — a missing account or
	// a metering failure must not block the API response.
	//
	// Gated on `won` so a concurrent pollworker terminal cannot cause a
	// double insert. The UNIQUE index on usage_events(higgsgo_job_id)
	// (migration 018) is a second line of defence if this gate ever
	// regresses.
	if won && s.Meter != nil {
		freshAcc, getErr := s.Store.Get(ctx, acc.ID)
		if getErr != nil || freshAcc == nil {
			// Fall back to the stale copy; Recorder will use upstream_cost
			// when it detects a zero or negative balance delta.
			freshAcc = acc
		}
		markup := s.resolveMarkup(ctx, req.APIKeyID)
		if err := s.Meter.OnJobTerminal(ctx, mJob, freshAcc, preBalance, markup); err != nil && s.Logger != nil {
			// domain.ErrUsageEventDuplicate can only surface here if the
			// F1 CAS gate above regressed AND the migration-018 UNIQUE
			// index caught the double insert. Not a real failure — log
			// at debug so operators are not paged for a race outcome.
			if errors.Is(err, domain.ErrUsageEventDuplicate) {
				s.Logger.Debug("metering skipped (duplicate)",
					slog.String("job_id", created.JobID))
			} else {
				s.Logger.Warn("metering failed",
					slog.String("job_id", created.JobID),
					slog.String("err", err.Error()))
			}
		}
	}

	// Webhook: fire-and-forget so the HTTP caller isn't blocked by delivery
	// latency or retries. Only fires when the caller supplied a callback URL
	// AND a Dispatcher is wired in — otherwise this branch is a no-op. The
	// pollworker owns the analogous fire on the async path.
	//
	// Also gated on `won` so we do not double-notify callers when the
	// pollworker witnessed the same terminal transition.
	if won && s.Webhooks != nil && req.CallbackURL != "" {
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

// buildBody assembles the payload dispatched to higgsfield.
//
// Layering order (later wins):
//  1. spec.ExampleBodyJSON — the on-disk body template captured from a
//     real successful upstream call. Provides sane defaults for every
//     param the model needs (seed, strength, style_id, aspect_ratio,
//     resolution, ...). When the template is missing (older loader /
//     model without a template file), we start with an empty map.
//  2. req.UserParams — caller-supplied overrides. These flow into the
//     nested params object and shadow template values verbatim; the
//     caller can also inject top-level keys (rarely needed).
//  3. Media injection — the pre-uploaded image/video/audio is spliced
//     into either params.medias[] or params.input_image based on
//     spec.MediaRole, replacing any placeholder from the template.
//  4. Hard defaults — use_unlim / use_seedream_bonus / application_slug.
//     These are always the higgsgo-owned values regardless of template.
//
// Callers that want the raw pass-through behaviour (no template merge)
// can zero out spec.ExampleBodyJSON before passing spec in — but every
// production path benefits from defaults because they close the "422
// missing param X" holes the WebUI cannot signal in advance.
func buildBody(spec *domain.ModelSpec, req GenerationRequest) map[string]any {
	top := map[string]any{}
	// Seed from the template. On any parse error we fall back to an
	// empty top-level object so pre-template behaviour is preserved.
	if spec != nil && spec.ExampleBodyJSON != "" {
		var tmpl map[string]any
		if err := json.Unmarshal([]byte(spec.ExampleBodyJSON), &tmpl); err == nil {
			top = tmpl
		}
	}
	// Extract or create the nested params object. Templates always wrap
	// params under "params"; a broken template that lacks it degrades
	// to the empty-map path so we don't send garbage upstream.
	params, ok := top["params"].(map[string]any)
	if !ok {
		params = make(map[string]any)
		top["params"] = params
	}
	// Merge user-supplied params on top of the template defaults.
	for k, v := range req.UserParams {
		params[k] = v
	}
	// Inject media if provided and the model wants it. The template may
	// carry an example media reference; user-provided media replaces it.
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
			params["input_image"] = mediaObj
		}
	}
	// Hard top-level defaults. These are higgsgo-owned so we always
	// stamp them last, overriding whatever the template carried.
	top["use_unlim"] = false
	top["use_seedream_bonus"] = false
	if spec != nil && spec.ApplicationSlug != "" {
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

// classifyCreateErr maps a core/upstream sentinel returned from CreateJob
// to a domain.ErrorType so the usage_events row records why the create
// failed. Mirrors the failover.Classify buckets but is intentionally
// stringly-typed on the resulting label — the errorType column is a free
// text tag for dashboards, not a routing signal.
func classifyCreateErr(err error) domain.ErrorType {
	switch {
	case errors.Is(err, domain.ErrUpstreamUnauthorized):
		return domain.ErrUpstream // session expired counts as upstream fail
	case errors.Is(err, domain.ErrUpstreamRateLimit):
		return domain.ErrRateLimit
	case errors.Is(err, domain.ErrUpstreamBadBody):
		return domain.ErrBody
	case errors.Is(err, domain.ErrUpstreamForbidden):
		return domain.ErrGate
	case errors.Is(err, domain.ErrUpstreamServerError):
		return domain.ErrUpstream
	case errors.Is(err, domain.ErrUpstreamTimeout):
		return domain.ErrTimeout
	}
	return domain.ErrUnknown
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

// isSpilloverEligible returns true when err comes from a group-scoped
// capacity or gate mismatch — the kind of failure that might succeed
// against a sibling group in the caller's binding list. ROADMAP P3-10.
//
// Deliberately excludes:
//   - ErrAPIKeyQuotaExceed: caller-level, no group will help.
//   - Upstream errors (rate limit, forbidden, etc.): the pool already
//     picked and CreateJob failed; spillover happens BEFORE that call.
//   - Anything unlisted: bail out — spillover is a best-effort
//     optimisation, not a way to paper over unknown failures.
func isSpilloverEligible(err error) bool {
	return errors.Is(err, domain.ErrGroupConcurrencyMax) ||
		errors.Is(err, domain.ErrGroupQuotaExhausted) ||
		errors.Is(err, domain.ErrNoEligibleAccount) ||
		errors.Is(err, domain.ErrModelBlocked) ||
		errors.Is(err, domain.ErrModelNotAllowed)
}

// enforceKeyGates rejects a request whose caller key has already used
// its monthly quota. Enforced pre-pick so a doomed request never
// consumes an in-flight slot on an account. See docs/ROADMAP.md P2-9.
//
// Uses the same "reserve headroom" rule as enforceGroupGates so a call
// that would exactly hit the quota still passes — the last credit of
// the month is spendable and the recorder's IncrementUsage side is
// what actually retires it. Overrun returns
// domain.ErrAPIKeyQuotaExceed which maps to HTTP 402 quota_exhausted.
//
// Zero quota means unlimited (the historical default for freshly
// minted keys) — the gate is a no-op in that case.
func enforceKeyGates(req GenerationRequest, estCostHundredths int64) error {
	if req.APIKeyMonthlyQuota <= 0 {
		return nil
	}
	if req.APIKeyMonthlyUsed+estCostHundredths > req.APIKeyMonthlyQuota {
		return domain.ErrAPIKeyQuotaExceed
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
