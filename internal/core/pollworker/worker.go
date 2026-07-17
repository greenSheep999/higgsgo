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

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Worker is a long-running poller for live jobs.
type Worker struct {
	Jobs     ports.JobStore
	Accounts ports.AccountStore
	Upstream *upstream.Client
	Logger   *slog.Logger

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

	// Internal.
	lastPolled sync.Map // upstream_job_id -> time.Time
}

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
	w.Logger.Info("job terminal",
		slog.String("job_id", j.ID),
		slog.String("model", j.ModelAlias),
		slog.String("status", string(finalStatus)),
		slog.Int64("latency_ms", latency),
		slog.String("result_url", fetched.ResultURL))

	if w.OnTerminal != nil {
		// Populate the outgoing struct so callbacks see final state.
		j.Status = finalStatus
		j.ResultURL = fetched.ResultURL
		j.Refunded = fetched.Refunded
		j.FinishedAt = now
		j.LatencyMS = latency
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
	if w.OnTerminal != nil {
		j.Status = domain.JobTimeout
		w.OnTerminal(j)
	}
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
