// Package metering records per-job accounting rows and drives the
// downstream billing paths (API key quota, group quota, upstream cost
// attribution).
//
// The Recorder is invoked at exactly one moment: the instant a job reaches
// a terminal state (completed / failed / refunded / timeout). Both the
// synchronous proxy service and the asynchronous pollworker fire it. The
// two paths differ in how they know the pre-job account balance:
//
//   - proxy.Service snapshots acc.SubscriptionBalance before create and
//     passes it as preBalance so the Recorder can compute the true actual
//     credits consumed as (preBalance - post.SubscriptionBalance).
//   - pollworker cannot easily snapshot preBalance (the create happened in
//     a previous request), so it passes preBalance=0 and the Recorder
//     falls back to job.UpstreamCost as the actual-credits estimate.
//
// This split accepts a small accuracy loss on async jobs in exchange for
// keeping the jobs table schema slim. A future migration can add a
// pre_balance_h column on jobs to make async attribution exact.
package metering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/observability"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// Recorder is the metering front-door. Callers construct one at startup and
// invoke OnJobTerminal from every place a job transitions to a terminal
// state.
type Recorder struct {
	Events   ports.UsageEventStore
	APIKeys  ports.APIKeyStore
	Accounts ports.AccountStore
	// Groups, when non-nil, receives an IncrementUsed call whenever a
	// terminal job carries a non-empty GroupID and non-zero charged
	// credits — this is what makes the group's monthly_credit_budget
	// (checked pre-pick by proxy.Service.enforceGroupGates) self-limit.
	// Wiring is optional: pre-P1-4 deployments that only care about
	// per-key accounting can leave this nil and the pre-pick budget
	// gate falls back to whatever monthly_credit_used the DB happens
	// to hold (typically zero, so the gate is effectively disabled).
	Groups ports.GroupStore
	// Jobs, when non-nil, receives an UpdateStatus call after the
	// usage_events row is inserted, back-filling actual/charged credits
	// on the jobs row so /admin/jobs and per-job UIs can display the
	// realized cost. Nil-safe: pre-migration deployments that only read
	// aggregates from usage_events can leave this unset and the jobs
	// table's credit columns stay NULL (harmless).
	Jobs   ports.JobStore
	Logger *slog.Logger
	// Metrics is optional; when non-nil, OnJobTerminal increments the
	// UsageCredits counter (labels: media_type, status) by the charged
	// credits (hundredths) after a successful Insert. A nil Metrics
	// makes the metering path metrics-free (used by tests and by
	// startup paths that have not built the collector yet).
	Metrics *observability.Metrics
}

// OnJobTerminal writes a usage_events row and applies the associated
// billing side effects (currently: increment APIKey monthly usage).
//
// Arguments:
//   - job:        the terminal job. Status / UpstreamCost / ResultURL etc.
//     must already be populated by the caller.
//   - account:    the account that ran the job, refreshed from the store
//     immediately before calling. Its SubscriptionBalance is
//     the post-job value used to compute actual credits.
//   - preBalance: subscription_balance recorded before job create, in
//     credits*100 units. Pass 0 when unknown; the Recorder
//     will fall back to job.UpstreamCost.
//   - markupPct:  billing markup multiplier. Values <= 0 are treated as
//     1.0 (no markup) so callers that do not fetch the API
//     key row can simply pass 0.
//
// The function never returns fatal errors to the caller: metering
// side-effects are best-effort and must not block job completion. Errors
// are logged and returned so tests can assert on them.
func (r *Recorder) OnJobTerminal(ctx context.Context, job *domain.Job, account *domain.Account, preBalance int64, markupPct float64) error {
	if r == nil || r.Events == nil {
		return nil
	}
	if job == nil {
		return fmt.Errorf("metering: nil job")
	}

	// 1. Compute actualCreditsH from the pre/post balance delta, clamped to
	//    non-negative. Fall back to job.UpstreamCost when preBalance is
	//    unknown or the delta is zero/negative (which happens for refunded
	//    jobs where higgsfield restores the balance).
	var actualCreditsH int64
	if account != nil && preBalance > 0 {
		delta := preBalance - account.SubscriptionBalance
		if delta > 0 {
			actualCreditsH = delta
		}
	}
	if actualCreditsH == 0 {
		actualCreditsH = job.UpstreamCost
	}
	if actualCreditsH < 0 {
		actualCreditsH = 0
	}

	// 2. Apply markup. <= 0 means "caller did not fetch the key row" — treat
	//    as pass-through.
	if markupPct <= 0 {
		markupPct = 1.0
	}
	chargedCreditsH := int64(float64(actualCreditsH) * markupPct)

	// 3. Assemble the event row.
	now := time.Now().UTC()
	accountID := ""
	if account != nil {
		accountID = account.ID
	}
	if accountID == "" {
		accountID = job.AccountID
	}
	event := &domain.UsageEvent{
		ID:                       idgen.NewID("ue"),
		TS:                       now,
		APIKeyID:                 job.APIKeyID,
		CPAPartnerID:             job.CPAPartnerID,
		GroupID:                  job.GroupID,
		AccountID:                accountID,
		ModelAlias:               job.ModelAlias,
		JST:                      job.JST,
		MediaType:                mediaTypeForJST(job.JST),
		UpstreamCost:             job.UpstreamCost,
		ActualCreditsHundredths:  actualCreditsH,
		ChargedCreditsHundredths: chargedCreditsH,
		MarkupPct:                markupPct,
		Status:                   job.Status,
		LatencyMS:                job.LatencyMS,
		PollCount:                job.PollCount,
		ErrorType:                job.ErrorType,
		HiggsgoJobID:             job.ID,
		UpstreamJobID:            job.UpstreamJobID,
		ResultURL:                job.ResultURL,
		BillingMonth:             now.Format("2006-01"),
		BillingDay:               now.Format("2006-01-02"),
	}

	if err := r.Events.Insert(ctx, event); err != nil {
		// domain.ErrUsageEventDuplicate is the F1 defence-in-depth
		// signal: the CAS gate in core/proxy + core/pollworker should
		// have already blocked this call, but if a caller ever bypasses
		// the gate the UNIQUE index on usage_events(higgsgo_job_id)
		// (migration 018) catches the duplicate here. Log at debug so
		// operators are not spammed by a benign race outcome, and
		// return the sentinel unchanged so the caller can distinguish
		// "already recorded" from a real store failure.
		if errors.Is(err, domain.ErrUsageEventDuplicate) {
			if r.Logger != nil {
				r.Logger.Debug("metering insert skipped (duplicate)",
					slog.String("job_id", job.ID))
			}
			return err
		}
		if r.Logger != nil {
			r.Logger.Warn("metering insert failed",
				slog.String("job_id", job.ID),
				slog.String("err", err.Error()))
		}
		return err
	}

	// 3.5. Back-fill actual/charged credits + markup on the jobs row so
	//      /admin/jobs and per-job UIs show realized cost. Best-effort:
	//      a failure here does not undo the usage_events insert. The SQL
	//      UpdateStatus helper only appends fields when non-zero, so a
	//      zero-cost terminal (rare) simply leaves the columns NULL.
	//
	//      ErrJobNotFound is silently swallowed: sync-path CreateJob
	//      failures (see proxy.Service.Generate) intentionally emit a
	//      usage_events row *without* a matching jobs row, using a
	//      locally-minted "cf" id. That is a feature, not a bug — the
	//      operator should see the failed request in usage reports even
	//      though upstream never issued a job id to persist.
	if r.Jobs != nil {
		meta := ports.JobMeta{
			ActualCreditsHundredths:  actualCreditsH,
			ChargedCreditsHundredths: chargedCreditsH,
		}
		if err := r.Jobs.UpdateStatus(ctx, job.ID, job.Status, meta); err != nil {
			if !errors.Is(err, domain.ErrJobNotFound) && r.Logger != nil {
				r.Logger.Warn("metering jobs backfill failed",
					slog.String("job_id", job.ID),
					slog.String("err", err.Error()))
			}
		}
	}

	// 3a. Increment the Prometheus usage-credits counter. Guarded by
	//     chargedCreditsH > 0 so we do not materialize noise label
	//     combinations for zero-cost terminals (e.g. refunded jobs with
	//     no upstream cost).
	if r.Metrics != nil && r.Metrics.UsageCredits != nil && chargedCreditsH > 0 {
		r.Metrics.UsageCredits.
			WithLabelValues(event.MediaType, string(event.Status)).
			Add(float64(chargedCreditsH))
	}

	// 4. Increment API key usage. Best-effort: a missing key or a stale row
	//    is not a metering failure.
	if job.APIKeyID != "" && r.APIKeys != nil && chargedCreditsH > 0 {
		if err := r.APIKeys.IncrementUsage(ctx, job.APIKeyID, chargedCreditsH); err != nil {
			if r.Logger != nil {
				r.Logger.Warn("metering apikey increment failed",
					slog.String("api_key_id", job.APIKeyID),
					slog.String("job_id", job.ID),
					slog.String("err", err.Error()))
			}
		}
	}

	// 5. Increment group monthly_credit_used. Best-effort like APIKeys:
	//    an admin who wiped the group budget mid-flight should not
	//    make the metering pipeline fail. Charged credits — not
	//    actual — because the operator sets the budget in the same
	//    unit the WebUI shows (post-markup credit spend). See
	//    docs/ROADMAP.md P1-4.
	if job.GroupID != "" && r.Groups != nil && chargedCreditsH > 0 {
		if err := r.Groups.IncrementUsed(ctx, job.GroupID, chargedCreditsH); err != nil {
			if r.Logger != nil {
				r.Logger.Warn("metering group increment failed",
					slog.String("group_id", job.GroupID),
					slog.String("job_id", job.ID),
					slog.String("err", err.Error()))
			}
		}
	}

	if r.Logger != nil {
		r.Logger.Info("usage recorded",
			slog.String("job_id", job.ID),
			slog.String("model", job.ModelAlias),
			slog.String("status", string(job.Status)),
			slog.Int64("actual_h", actualCreditsH),
			slog.Int64("charged_h", chargedCreditsH),
			slog.Float64("markup", markupPct))
	}
	return nil
}

// mediaTypeForJST returns "image" | "video" | "audio" derived from JST
// substrings. Matches the heuristic in internal/api/v1/jobs.go so admin
// dashboards stay consistent with the /v1 output-object label.
func mediaTypeForJST(jst string) string {
	switch {
	case containsAny(jst,
		"video", "seedance", "kling", "veo3", "wan", "sora", "cinema",
		"marketing", "grok_video", "hailuo", "happy_horse", "gemini_omni",
		"infinite_talk", "hf_fnf"):
		return "video"
	case containsAny(jst,
		"speech", "audio", "sonilo", "mirelo", "text2speech",
		"clip_transcriber"):
		return "audio"
	default:
		return "image"
	}
}

// containsAny reports whether s contains any of the given substrings.
// Kept private so callers stay funnelled through mediaTypeForJST.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
