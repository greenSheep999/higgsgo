// Package regression runs a background ticker that periodically re-probes a
// sample of registered models to detect silent quality regressions (a model
// that used to complete now returning B_upstream_fail, a 402 quota gate that
// appeared upstream, an endpoint that started returning 422, ...).
//
// Design notes:
//
//   - Only image-output models are probed. Video probes are 20-100x more
//     expensive both in credits and wall-clock time; those get their own
//     weekly cadence via a separate ticker (see docs/ARCHITECTURE.md §3.5,
//     "x1_recheck.go").
//   - Each tick picks SampleSize models — preferring those with the oldest
//     Latest.CheckedAt, falling back to unprobed models to fill the batch —
//     and dispatches them through a caller-supplied ProxyInvoker. The
//     invoker abstraction lets tests inject a fake proxy without spinning
//     up upstream HTTP mocks.
//   - Failures are isolated per model: one flaky endpoint never blocks the
//     rest of the batch. Every outcome is persisted to model_health so
//     downstream dashboards can compute streak length and last verdict.
//   - Concurrency is bounded by a semaphore. SampleSize is the tick-size
//     cap; Concurrency is the in-flight cap during that tick.
package regression

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// defaultInterval is the tick cadence when Interval is left at zero.
// Once a day matches docs/ARCHITECTURE.md §3.5 ("a_regression at 06:00").
const defaultInterval = 24 * time.Hour

// defaultConcurrency caps parallel per-model probes when Concurrency is
// left at zero. Kept low so a probe burst does not stampede upstream.
const defaultConcurrency = 2

// defaultSampleSize is the number of models probed per tick when
// SampleSize is left at zero.
const defaultSampleSize = 5

// defaultProbePrompt is the text prompt used for image probes. Kept short
// and neutral so it produces a small cheap image and cannot trip content
// filters.
const defaultProbePrompt = "hello world"

// ProxyInvoker is the narrow slice of proxy.Service used by the regression
// ticker. Declared as a local interface so tests can inject a fake without
// pulling in the full proxy dependency graph.
type ProxyInvoker interface {
	Generate(ctx context.Context, req proxy.GenerationRequest) (*proxy.GenerationResponse, error)
}

// Ticker periodically probes a sample of image-output models and records
// the outcome to a ModelHealthStore. Wire it into main.go with `go
// tk.Run(ctx)` after the storage and proxy dependencies are built.
type Ticker struct {
	// Health persists every probe outcome. Required.
	Health ports.ModelHealthStore

	// Registry enumerates candidate models. Required.
	Registry ports.ModelRegistry

	// Proxy dispatches each probe request. Required (nil disables the
	// tick, which is what SkipUpstream also does).
	Proxy ProxyInvoker

	// Logger for structured tick logs. Optional; nil silences all logs.
	Logger *slog.Logger

	// Interval between ticks. Default 24h.
	Interval time.Duration

	// Concurrency caps in-flight probes within a single tick. Default 2.
	Concurrency int

	// SampleSize is the number of models probed per tick. Default 5.
	SampleSize int

	// Prompt overrides the default probe prompt. Empty falls back to the
	// package default. Only used when constructing the GenerationRequest.
	Prompt string

	// GroupID scopes the pool pick to a specific account group. Empty
	// selects the "default" group (proxy.Service handles the fallback).
	GroupID string

	// SkipUpstream, when true, records probe attempts but never actually
	// calls Proxy.Generate. Every persisted row lands with verdict = "pending"
	// so downstream dashboards can tell the ticker fired without consuming
	// credits. Intended for dev/test environments; production leaves this
	// false so probes hit upstream.
	SkipUpstream bool
}

// Run blocks until ctx is canceled. Intended to be invoked as
// `go tk.Run(ctx)` from the process boot path.
func (t *Ticker) Run(ctx context.Context) {
	t.applyDefaults()

	if t.Logger != nil {
		t.Logger.Info("regression ticker starting",
			slog.Duration("interval", t.Interval),
			slog.Int("concurrency", t.Concurrency),
			slog.Int("sample_size", t.SampleSize),
			slog.Bool("skip_upstream", t.SkipUpstream),
		)
	}

	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()

	// One immediate pass so a freshly booted process does not wait an
	// entire interval before observing model health. Matches refresher.go.
	t.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			if t.Logger != nil {
				t.Logger.Info("regression ticker stopping")
			}
			return
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

// TriggerOnce runs a single probe pass synchronously. Intended for admin
// endpoints that want to force an immediate regression sweep without
// waiting for the ticker interval. Failures inside tick / probeOne are
// already logged per-model, so this wrapper has nothing to return.
func (t *Ticker) TriggerOnce(ctx context.Context) {
	t.applyDefaults()
	t.tick(ctx)
}

// applyDefaults fills zero-valued config fields with package defaults.
// Broken out so tests can call it directly instead of going through Run.
func (t *Ticker) applyDefaults() {
	if t.Interval <= 0 {
		t.Interval = defaultInterval
	}
	if t.Concurrency <= 0 {
		t.Concurrency = defaultConcurrency
	}
	if t.SampleSize <= 0 {
		t.SampleSize = defaultSampleSize
	}
	if t.Prompt == "" {
		t.Prompt = defaultProbePrompt
	}
}

// tick runs one probe pass. Exposed to tests via a single-shot entry point
// (see regression_test.go) so they can assert without dealing with real
// time.Ticker cadence.
func (t *Ticker) tick(ctx context.Context) {
	t.applyDefaults()

	candidates := t.pickCandidates(ctx)
	if len(candidates) == 0 {
		if t.Logger != nil {
			t.Logger.Debug("regression tick: no candidates")
		}
		return
	}
	if t.Logger != nil {
		t.Logger.Info("regression tick",
			slog.Int("candidates", len(candidates)),
		)
	}

	sem := make(chan struct{}, t.Concurrency)
	var wg sync.WaitGroup
	for i := range candidates {
		spec := candidates[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t.probeOne(ctx, spec)
		}()
	}
	wg.Wait()
}

// pickCandidates enumerates image-output models and returns the SampleSize
// entries with the oldest Latest.CheckedAt. Never-probed models sort as
// "oldest" (nil row) so they are picked first — that guarantees a freshly
// registered model gets covered on the very next tick.
func (t *Ticker) pickCandidates(ctx context.Context) []*domain.ModelSpec {
	all := t.Registry.List(ports.ModelFilter{Output: "image"})

	// Belt-and-suspenders JST filter: some jsonstatic registries used to
	// mis-classify Output. Cross-check with the "image" substring in the
	// jst so we never accidentally probe an expensive video model.
	filtered := make([]*domain.ModelSpec, 0, len(all))
	for _, spec := range all {
		if spec == nil {
			continue
		}
		if !isImageModel(spec) {
			continue
		}
		filtered = append(filtered, spec)
	}
	if len(filtered) == 0 {
		return nil
	}

	type entry struct {
		spec      *domain.ModelSpec
		checkedAt time.Time
		known     bool
	}
	entries := make([]entry, 0, len(filtered))
	for _, spec := range filtered {
		e := entry{spec: spec}
		if t.Health != nil {
			row, err := t.Health.Latest(ctx, spec.JST)
			if err != nil {
				if t.Logger != nil {
					t.Logger.Warn("regression latest lookup",
						slog.String("jst", spec.JST),
						slog.String("err", err.Error()))
				}
			} else if row != nil {
				e.checkedAt = row.CheckedAt
				e.known = true
			}
		}
		entries = append(entries, e)
	}

	// Oldest-first ordering. Unprobed (!known) sorts strictly before any
	// probed model; among probed, older checked_at wins. Stable JST tie-
	// break so tests get a deterministic sample.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].known != entries[j].known {
			return !entries[i].known
		}
		if !entries[i].checkedAt.Equal(entries[j].checkedAt) {
			return entries[i].checkedAt.Before(entries[j].checkedAt)
		}
		return entries[i].spec.JST < entries[j].spec.JST
	})

	n := t.SampleSize
	if n > len(entries) {
		n = len(entries)
	}
	out := make([]*domain.ModelSpec, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, entries[i].spec)
	}
	return out
}

// isImageModel returns true when the spec should be routed through the
// image-cheap probe path. Guards against a mis-classified registry entry
// (Output=="image" but jst obviously video) by also checking the jst
// substring.
func isImageModel(spec *domain.ModelSpec) bool {
	if spec.Output != "image" {
		return false
	}
	// Reject if jst clearly names a video/audio path — belt-and-suspenders
	// so a badly categorized entry cannot burn video credits.
	lower := strings.ToLower(spec.JST)
	if strings.Contains(lower, "video") || strings.Contains(lower, "veo") || strings.Contains(lower, "animate") {
		return false
	}
	return true
}

// probeOne dispatches a single generation request and records the outcome
// to Health. Errors are logged at warn level but never propagated: a bad
// probe should not tear down the tick.
func (t *Ticker) probeOne(ctx context.Context, spec *domain.ModelSpec) {
	start := time.Now()

	// SkipUpstream lets test / dev environments exercise the store path
	// without burning credits. We still write a row so operators can see
	// that the ticker fired.
	if t.SkipUpstream || t.Proxy == nil {
		verdict := domain.JobPending
		if err := t.Health.Insert(ctx, spec.JST, start.UTC(), verdict, 0, 0, 0); err != nil && t.Logger != nil {
			t.Logger.Warn("regression insert (skip)",
				slog.String("jst", spec.JST),
				slog.String("err", err.Error()))
		}
		if t.Logger != nil {
			t.Logger.Debug("regression probe skipped",
				slog.String("jst", spec.JST),
				slog.String("alias", spec.Alias))
		}
		return
	}

	req := proxy.GenerationRequest{
		Model: spec.Alias,
		UserParams: map[string]any{
			"prompt": t.Prompt,
		},
		SyncRequested: true,
		GroupID:       t.GroupID,
	}
	resp, err := t.Proxy.Generate(ctx, req)
	elapsed := time.Since(start)

	verdict := domain.JobFailed
	httpStatus := 0
	cost := int64(0)
	pollSec := int(elapsed.Seconds())
	if err == nil && resp != nil {
		switch domain.JobStatus(resp.Status) {
		case domain.JobCompleted:
			if resp.ResultURL != "" {
				verdict = domain.JobCompleted
			}
		case domain.JobQueued, domain.JobRunning:
			// Sync probe returned before terminal state. Treat as
			// still-pending so a subsequent tick will re-check. We do
			// not overwrite Cost / HTTPStatus (both unknown).
			verdict = domain.JobPending
		case domain.JobRefunded:
			// Refund means upstream_fail then bookkeeping — treat as
			// failed for probe purposes so the regression alert fires.
			verdict = domain.JobFailed
		}
		cost = resp.Cost
	}

	if err != nil && t.Logger != nil {
		t.Logger.Warn("regression probe error",
			slog.String("jst", spec.JST),
			slog.String("alias", spec.Alias),
			slog.String("err", err.Error()),
			slog.Duration("elapsed", elapsed),
		)
	}

	if insertErr := t.Health.Insert(ctx, spec.JST, start.UTC(), verdict, httpStatus, cost, pollSec); insertErr != nil && t.Logger != nil {
		t.Logger.Warn("regression insert",
			slog.String("jst", spec.JST),
			slog.String("err", insertErr.Error()))
	}

	if t.Logger != nil {
		t.Logger.Info("regression probe done",
			slog.String("jst", spec.JST),
			slog.String("alias", spec.Alias),
			slog.String("verdict", string(verdict)),
			slog.Duration("elapsed", elapsed),
		)
	}
}

// Compile-time guard: real proxy.Service satisfies ProxyInvoker.
// Kept here so a refactor of proxy.Service surfaces this package first.
var _ ProxyInvoker = (*proxy.Service)(nil)
