// Package pollworker runs a background loop that polls every live job in
// the JobStore against higgsfield's status endpoint until it reaches a
// terminal state or its deadline expires.
//
// This decouples "job creation succeeded" from "generation finished".
// Slow models (wan2_2_animate family, cinematic_studio_video, seedance_2_0
// under load) may take 30+ minutes upstream but higgsfield never streams
// results — you must poll. Without this worker, higgsgo would either
// block the HTTP request for 30 minutes or lose track of the job
// (the failure mode of the earlier Python tests, misclassifying B-class
// models as failed when they were merely slow).
//
// Design:
//
//   - one worker tick every TickInterval (default 8s)
//   - each tick: JobStore.ListPending() → for each job, fetch status
//   - per-job spacing: never poll the same upstream job faster than
//     PerJobMinInterval (default 5s) — respects higgsfield's rate limits
//   - hard deadline per job: JobStore records were request_ts; when
//     time.Now().Sub(request_ts) > JobDeadline, mark timeout
package pollworker

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/failover"
	"github.com/greensheep999/higgsgo/internal/core/metering"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// UpstreamPoller is the narrow surface the worker needs from an upstream
// client. Exposed as an interface (rather than a concrete *upstream.Client)
// so tests can inject a fake without spinning up a JWT minter + JA3 HTTP
// stack. *upstream.Client satisfies it directly.
type UpstreamPoller interface {
	FetchStatus(ctx context.Context, account *domain.Account, jobID string) (*upstream.StatusResponse, error)
	FetchJob(ctx context.Context, account *domain.Account, jobID string) (*upstream.FetchResponse, error)
}

// MeterSink is what the worker needs from a metering.Recorder. Defined as
// an interface for the same reason as UpstreamPoller: hermetic tests.
// *metering.Recorder satisfies it directly.
type MeterSink interface {
	OnJobTerminal(ctx context.Context, job *domain.Job, account *domain.Account, preBalance int64, markupPct float64) error
}

// WebhookSink is what the worker needs from a webhook.Dispatcher. See the
// note on UpstreamPoller for the rationale. *webhook.Dispatcher satisfies
// it directly.
type WebhookSink interface {
	Fire(url string, job *domain.Job)
}

// Worker is a long-running poller for live jobs.
type Worker struct {
	Jobs     ports.JobStore
	Accounts ports.AccountStore
	Upstream UpstreamPoller
	Logger   *slog.Logger

	// APIKeys, when non-nil, is consulted at the terminal transition to look
	// up APIKey.MarkupPct so the Recorder can apply the operator-configured
	// markup on top of the actual upstream credits. Lookup failures fall
	// back to markup=0 (Recorder treats 0 as multiplier 1.0). Metering
	// must never block on this.
	APIKeys ports.APIKeyStore

	// TickInterval is how often the worker wakes up. Default 8s.
	TickInterval time.Duration

	// PerJobMinInterval is the shortest allowed gap between polls of the
	// same upstream job. Default 5s. Helps stay under upstream rate limits.
	PerJobMinInterval time.Duration

	// JobDeadline is the total wall-clock time a job may spend queued or
	// in_progress before we give up and mark it timeout. Default 60min —
	// long enough for wan_animate family which we've measured completing
	// after 30-40 minutes upstream.
	JobDeadline time.Duration

	// OnTerminal fires after a job reaches completed/failed/timeout state.
	// The Worker holds no reference to it once fired; consumers use it to
	// send webhooks or emit metrics.
	OnTerminal func(job *domain.Job)

	// Meter, when non-nil, receives a usage event after each terminal job.
	// The worker path now passes j.PreBalanceH (persisted at create time on
	// the jobs row) so the Recorder can compute exact credits consumed via
	// the (pre - post) delta path. When PreBalanceH is zero the Recorder
	// falls back to job.UpstreamCost. Metering failures are logged but
	// never propagated — accounting must not block a terminal transition.
	Meter MeterSink

	// Webhooks, when non-nil, delivers a signed terminal-state notification
	// to the caller-supplied CallbackURL after each terminal transition
	// (completed / failed / refunded / timeout). Fire is non-blocking; the
	// Dispatcher owns retry and drain-on-close.
	Webhooks WebhookSink

	// Failover, when non-nil, receives account-attributable failure
	// / success signals so the failover controller can pull dead
	// accounts out of rotation. Nil-safe on every call site: the
	// controller's methods short-circuit on a nil receiver.
	Failover *failover.Controller

	// Internal.
	lastPolled sync.Map // upstream_job_id -> time.Time
}

// Ensure the concrete types the production wiring uses still satisfy the
// interfaces above. These compile-time assertions guard against drift.
var (
	_ UpstreamPoller = (*upstream.Client)(nil)
	_ MeterSink      = (*metering.Recorder)(nil)
)

// Defaults returns a Worker populated with sensible defaults. The caller
// must set Jobs, Accounts, Upstream, and Logger.
func Defaults() *Worker {
	return &Worker{
		TickInterval:      8 * time.Second,
		PerJobMinInterval: 5 * time.Second,
		JobDeadline:       60 * time.Minute,
	}
}

// Run blocks until ctx is canceled. Meant to be invoked as `go w.Run(ctx)`.
func (w *Worker) Run(ctx context.Context) {
	if w.TickInterval == 0 {
		w.TickInterval = 8 * time.Second
	}
	if w.PerJobMinInterval == 0 {
		w.PerJobMinInterval = 5 * time.Second
	}
	if w.JobDeadline == 0 {
		w.JobDeadline = 60 * time.Minute
	}
	w.Logger.Info("pollworker starting",
		slog.Duration("tick", w.TickInterval),
		slog.Duration("per_job_min", w.PerJobMinInterval),
		slog.Duration("deadline", w.JobDeadline),
	)

	ticker := time.NewTicker(w.TickInterval)
	defer ticker.Stop()

	// Kick off one immediate tick so newly created jobs aren't stalled by
	// the initial wait.
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			w.Logger.Info("pollworker stopping")
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// tick performs one polling pass over live jobs.
func (w *Worker) tick(ctx context.Context) {
	live, err := w.Jobs.ListPending(ctx)
	if err != nil {
		w.Logger.Warn("pollworker list pending", slog.String("err", err.Error()))
		return
	}
	if len(live) == 0 {
		return
	}
	w.Logger.Debug("pollworker tick", slog.Int("live_jobs", len(live)))

	now := time.Now()
	for i := range live {
		j := &live[i]
		w.pollOne(ctx, j, now)
	}
}

// pollOne polls a single job. Handles deadline, per-job cadence, and
// terminal transitions.
func (w *Worker) pollOne(ctx context.Context, j *domain.Job, now time.Time) {
	// Deadline check.
	if !j.RequestTS.IsZero() && now.Sub(j.RequestTS) > w.JobDeadline {
		w.markTimeout(ctx, j)
		return
	}

	// Per-job spacing check.
	if j.UpstreamJobID == "" {
		return // nothing to poll yet
	}
	if last, ok := w.lastPolled.Load(j.UpstreamJobID); ok {
		if lastT, ok := last.(time.Time); ok && now.Sub(lastT) < w.PerJobMinInterval {
			return
		}
	}

	acc, err := w.Accounts.Get(ctx, j.AccountID)
	if err != nil {
		w.Logger.Warn("pollworker fetch account",
			slog.String("job_id", j.ID),
			slog.String("account_id", j.AccountID),
			slog.String("err", err.Error()))
		return
	}

	status, err := w.Upstream.FetchStatus(ctx, acc, j.UpstreamJobID)
	w.lastPolled.Store(j.UpstreamJobID, now)
	if err != nil {
		w.Logger.Debug("pollworker fetch status",
			slog.String("job_id", j.ID),
			slog.String("err", err.Error()))
		// Feed the failover controller with the poll-time error so a
		// dead account (persistent 401 / DataDome challenge) doesn't
		// silently rack up polls until its deadline. Nil-safe.
		w.Failover.RecordError(ctx, j.AccountID, err, err.Error())
		// Increment poll count anyway so operators can see we tried.
		_ = w.Jobs.UpdateStatus(ctx, j.ID, j.Status, ports.JobMeta{PollCount: j.PollCount + 1})
		return
	}

	// Status transition detected?
	if !isTerminal(status.Status) {
		if string(j.Status) != status.Status {
			_ = w.Jobs.UpdateStatus(ctx, j.ID, domain.JobStatus(status.Status), ports.JobMeta{
				PollCount: j.PollCount + 1,
			})
			w.Logger.Info("job status changed",
				slog.String("job_id", j.ID),
				slog.String("model", j.ModelAlias),
				slog.String("from", string(j.Status)),
				slog.String("to", status.Status))
		}
		return
	}

	// Terminal: fetch full job details to grab result URL / refund flag.
	fetched, err := w.Upstream.FetchJob(ctx, acc, j.UpstreamJobID)
	if err != nil {
		w.Logger.Warn("pollworker fetch terminal job",
			slog.String("job_id", j.ID),
			slog.String("err", err.Error()))
		return
	}

	latency := int64(0)
	if !j.RequestTS.IsZero() {
		latency = now.Sub(j.RequestTS).Milliseconds()
	}
	finalStatus := domainStatus(status.Status, fetched.Refunded)
	meta := ports.JobMeta{
		ResultURL: fetched.ResultURL,
		LatencyMS: latency,
		PollCount: j.PollCount + 1,
	}
	if fetched.Refunded {
		meta.Refunded = true
	}
	if status.Status == "failed" {
		meta.ErrorType = domain.ErrUpstream
		meta.ErrorDetail = "upstream reported failed status"
	}
	if err := w.Jobs.UpdateStatus(ctx, j.ID, finalStatus, meta); err != nil {
		w.Logger.Warn("pollworker update terminal",
			slog.String("job_id", j.ID),
			slog.String("err", err.Error()))
		return
	}
	// Failover feedback: a "completed" upstream terminal is a healthy
	// run and clears the account's fail streak. Any other terminal
	// (failed / nsfw / terminated) is a content-level outcome and is
	// intentionally NOT fed back — those tell us nothing about the
	// health of the account itself. Nil-safe.
	if status.Status == "completed" {
		w.Failover.RecordSuccess(ctx, j.AccountID)
	}
	w.Logger.Info("job terminal",
		slog.String("job_id", j.ID),
		slog.String("model", j.ModelAlias),
		slog.String("status", string(finalStatus)),
		slog.Int64("latency_ms", latency),
		slog.String("result_url", fetched.ResultURL))

	// Populate the outgoing struct so callbacks + metering see final state.
	j.Status = finalStatus
	j.ResultURL = fetched.ResultURL
	j.Refunded = fetched.Refunded
	j.FinishedAt = now
	j.LatencyMS = latency

	// Metering: forward the pre-create balance snapshot persisted on the
	// jobs row so the Recorder can compute exact credits consumed via the
	// (pre - post) delta path. A zero PreBalanceH means the job predates
	// migration 003 (or was created without a snapshot) and the Recorder
	// falls back to job.UpstreamCost — never block terminal transitions
	// on accounting.
	if w.Meter != nil {
		freshAcc, getErr := w.Accounts.Get(ctx, j.AccountID)
		if getErr != nil || freshAcc == nil {
			freshAcc = acc
		}
		markup := w.resolveMarkup(ctx, j.APIKeyID)
		if err := w.Meter.OnJobTerminal(ctx, j, freshAcc, j.PreBalanceH, markup); err != nil {
			w.Logger.Warn("metering failed",
				slog.String("job_id", j.ID),
				slog.String("err", err.Error()))
		}
	}

	// Webhook: fire-and-forget delivery to the caller-supplied URL. Only
	// fires when both the job carries a CallbackURL and a Dispatcher is
	// wired in. Sync-path webhooks are handled in core/proxy so the two
	// paths do not double-fire (only one of them observes the terminal
	// transition for any given job).
	if w.Webhooks != nil && j.CallbackURL != "" {
		w.Webhooks.Fire(j.CallbackURL, j)
	}

	if w.OnTerminal != nil {
		w.OnTerminal(j)
	}
}

// markTimeout sets a job to timeout state and clears its poll gate.
func (w *Worker) markTimeout(ctx context.Context, j *domain.Job) {
	err := w.Jobs.UpdateStatus(ctx, j.ID, domain.JobTimeout, ports.JobMeta{
		ErrorType:   domain.ErrTimeout,
		ErrorDetail: "job did not reach terminal state within deadline",
		LatencyMS:   time.Since(j.RequestTS).Milliseconds(),
	})
	if err != nil {
		w.Logger.Warn("pollworker mark timeout",
			slog.String("job_id", j.ID),
			slog.String("err", err.Error()))
		return
	}
	w.Logger.Warn("job timeout",
		slog.String("job_id", j.ID),
		slog.String("model", j.ModelAlias),
		slog.Duration("age", time.Since(j.RequestTS)))
	j.Status = domain.JobTimeout
	j.LatencyMS = time.Since(j.RequestTS).Milliseconds()

	// Metering: emit even for timeouts so operators can attribute credits
	// that upstream may still charge. Account lookup is best-effort;
	// Recorder tolerates a nil-safe fallback path. Pass the persisted
	// PreBalanceH so the delta path can still apply when upstream actually
	// consumed credits before we gave up.
	if w.Meter != nil {
		if acc, aerr := w.Accounts.Get(ctx, j.AccountID); aerr == nil && acc != nil {
			markup := w.resolveMarkup(ctx, j.APIKeyID)
			if err := w.Meter.OnJobTerminal(ctx, j, acc, j.PreBalanceH, markup); err != nil {
				w.Logger.Warn("metering failed",
					slog.String("job_id", j.ID),
					slog.String("err", err.Error()))
			}
		}
	}

	// Webhook: notify the caller that the job timed out. Same fire-and-forget
	// contract as the completion path.
	if w.Webhooks != nil && j.CallbackURL != "" {
		w.Webhooks.Fire(j.CallbackURL, j)
	}

	if w.OnTerminal != nil {
		w.OnTerminal(j)
	}
}

// resolveMarkup looks up APIKey.MarkupPct for the caller behind this job.
// Returns 0 when APIKeys is nil, the job carries no api_key_id, the key row
// is missing, or the lookup errors — Recorder treats 0 as multiplier 1.0.
// Failures are logged at warn level but never propagated: metering must not
// block a terminal transition.
func (w *Worker) resolveMarkup(ctx context.Context, apiKeyID string) float64 {
	if w.APIKeys == nil || apiKeyID == "" {
		return 0
	}
	k, err := w.APIKeys.Get(ctx, apiKeyID)
	if err != nil {
		w.Logger.Warn("resolve markup failed",
			slog.String("api_key_id", apiKeyID),
			slog.String("err", err.Error()))
		return 0
	}
	if k == nil {
		return 0
	}
	return k.MarkupPct
}

// isTerminal matches upstream's terminal statuses.
func isTerminal(s string) bool {
	switch s {
	case "completed", "failed", "nsfw", "terminated":
		return true
	}
	return false
}

// domainStatus maps a raw upstream status + refund flag to a JobStatus.
func domainStatus(upstreamStatus string, refunded bool) domain.JobStatus {
	switch upstreamStatus {
	case "completed":
		return domain.JobCompleted
	case "failed", "terminated", "nsfw":
		if refunded {
			return domain.JobRefunded
		}
		return domain.JobFailed
	}
	return domain.JobStatus(upstreamStatus)
}

// EnsureBodyJSON is a small helper for callers that want to store the
// request body as a compact JSON string in Job.RequestBodyJSON. Kept here
// so it's next to the store logic.
func EnsureBodyJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
