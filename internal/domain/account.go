// Package domain holds pure business types shared across the codebase.
// It has zero external dependencies and no imports outside the standard library.
// See docs/ARCHITECTURE.md and docs/CONVENTIONS.md.
package domain

import "time"

// PlanType is the account tier reported by the higgsfield /user endpoint.
// Values are lowercase because that is what upstream returns.
type PlanType string

const (
	PlanFree     PlanType = "free"
	PlanStarter  PlanType = "starter"
	PlanBasic    PlanType = "basic"
	PlanPro      PlanType = "pro"
	PlanPlus     PlanType = "plus"
	PlanUltimate PlanType = "ultimate"
	PlanTeam     PlanType = "team"
	PlanUltra    PlanType = "ultra"
	PlanScale    PlanType = "scale"
	PlanCreator  PlanType = "creator"
	PlanEnt      PlanType = "enterprise"
)

// IsPaid reports whether the plan can run any model beyond the free/starter
// gate. Empirically established on 2026-07-17 (see data/reference/sealed.json).
func (p PlanType) IsPaid() bool {
	switch p {
	case PlanFree, PlanStarter, PlanBasic:
		return false
	default:
		return true
	}
}

// TierRank returns a coarse ordering for minimum-plan routing. It is separate
// from IsPaid because some upstream gates require Basic-or-newer, while the
// older "paid" gate intentionally starts at Pro.
func (p PlanType) TierRank() int {
	switch p {
	case PlanStarter:
		return 1
	case PlanBasic:
		return 2
	case PlanPro:
		return 3
	case PlanPlus:
		return 4
	case PlanUltra, PlanUltimate, PlanScale, PlanCreator, PlanTeam, PlanEnt:
		return 5
	default:
		return 0
	}
}

// MeetsMinimum reports whether this account plan satisfies a model's explicit
// minimum plan floor. Empty / free minimums mean no plan floor.
func (p PlanType) MeetsMinimum(min PlanType) bool {
	if min == "" || min == PlanFree {
		return true
	}
	minRank := min.TierRank()
	if minRank == 0 {
		return true
	}
	return p.TierRank() >= minRank
}

// AccountStatus is the lifecycle state we track locally.
//
// suspended / banned carry manual, operator-initiated semantics (e.g.,
// "we paused this account on purpose" / "upstream banned this account").
// throttled / disabled are populated exclusively by the failover
// controller (see core/failover) — never reuse those two for manual
// pauses or the recovery ticker will fight the operator.
type AccountStatus string

const (
	StatusActive    AccountStatus = "active"
	StatusSuspended AccountStatus = "suspended"
	StatusExpired   AccountStatus = "expired"
	StatusBanned    AccountStatus = "banned"
	// StatusThrottled — set by core/failover.RecordThrottle when the
	// controller decides an account has been rate-limited or
	// challenged by higgsfield's anti-bot layer. Paired with
	// Account.ThrottledUntil; the Recoverer goroutine flips the row
	// back to active once that deadline passes.
	StatusThrottled AccountStatus = "throttled"
	// StatusDisabled — set by core/failover on the terminal edge
	// (consecutive-failure limit or repeated-blacklist evict). Only
	// an operator can bring a disabled account back to active via
	// POST /admin/accounts/{id}/recover; the Recoverer never touches
	// this state on its own.
	StatusDisabled AccountStatus = "disabled"
)

// Account is a higgsfield account we can drive on behalf of a user.
// The struct maps directly to the accounts table (see internal/adapters/storage).
type Account struct {
	// Identity (harvested at registration/login).
	ID               string // clerk user_id, e.g. "user_3GPd..."
	Email            string
	Password         string // encrypted at rest by the storage adapter
	SessionID        string // clerk session_id, used to mint JWTs
	CookiesJSON      string // JSON blob of all cookies (including __session, __client, datadome)
	UserAgent        string // UA captured at login; must be reused for all upstream calls
	DataDomeClientID string // x-datadome-clientid header value
	WorkspaceID      string

	// Plan and API-level permissions (from /user endpoint).
	// has_unlim allows use_unlim:true flag on regular endpoints but is
	// NOT sufficient to reach /jobs/v2/*_unlimited endpoints — those require
	// Ultra or higher. See data/reference/unlimited-semantics.json.
	PlanType           PlanType
	HasUnlim           bool
	HasFlexUnlim       bool
	IsProVeo3Available bool
	Cohort             string

	// Balance (refreshed periodically).
	// SubscriptionBalance is stored in higgsfield's internal unit (credits * 100).
	// CreditsBalance briefly drops to 0 during credit freeze after a job creates —
	// do NOT use it for budget checks. Use SubscriptionBalance.
	SubscriptionBalance int64
	CreditsBalance      int64
	TotalPlanCredits    int64
	PlanEndsAt          time.Time

	// Local state.
	Status        AccountStatus
	InFlightJobs  int32 // current queued + in_progress. concurrent_jobs_limit=6 upstream.
	LastBalanceAt time.Time
	LastUsedAt    time.Time
	LastFailedAt  time.Time
	FailStreak    int

	// ThrottledUntil, when non-zero and in the future, means the failover
	// controller has cooled this account down (status == StatusThrottled).
	// PickAndLock skips the row until now() >= ThrottledUntil, and the
	// Recoverer goroutine flips status back to active once the deadline
	// passes. See migration 013.
	ThrottledUntil time.Time
	// StatusReason is a short opcode ("consec_fail", "risk_marker",
	// "auth_failed", ...) written alongside status transitions so the
	// admin surface can render "why" without joining the events table
	// for every list rendering. Empty when the row was never touched
	// by the failover controller.
	StatusReason string

	// Upstream-derived lifecycle signals (migration 021). These mirror
	// higgsfield's own flags and are orthogonal to Status: they do NOT
	// gate pool eligibility (status='active' remains the sole入池 gate).
	// The replenish alerter reads them; an operator decides whether to
	// pull a flagged account.
	//   GraceStatus:  normalized /workspaces/notice token (grace /
	//                 enforcement / access_lose / ...), "" when none.
	//   BlockedAt / SuspendedAt: raw upstream timestamp strings, ""
	//                 when unset; non-empty = flagged.
	//   IsPaused:     account paused (or pause scheduled) upstream.
	GraceStatus  string
	BlockedAt    string
	SuspendedAt  string
	IsPaused     bool

	// Optional IP binding for models that require sticky IP (image2video_extend etc.).
	BoundProxyURL string

	// Priority is an operator-managed sort hint used by the pool router
	// to prefer some accounts over others (higher = picked first).
	// Default 0; range is [-1_000, 1_000] enforced at the handler layer.
	// See migration 010_accounts_priority.sql.
	Priority int

	// MaxConcurrent overrides the global upstream concurrency cap (6) for
	// this account. 0 means "use the global default".
	MaxConcurrent int
	// Note is a free-form operator memo for this account.
	Note string
	// Source records how this account entered the pool (e.g. "manual",
	// "imported", "registered").
	Source string

	RegisteredAt time.Time
	ImportedAt   time.Time

	// FreeQuota carries the per-family free-generation counters returned
	// by GET /user. Each field is a float upstream (some values come back
	// as 0.4 for partial credits) so we preserve REAL precision on disk
	// rather than rounding into hundredths. Refreshed by the balance
	// refresher on every tick; read by the load-balance router when the
	// operator opts into `load_balance.prefer_free_quota`.
	FreeQuota FreeQuotaCounters
}

// FreeQuotaCounters mirrors the per-family free-generation counters
// returned by GET /user. Field names match the JSON keys higgsfield's
// API uses so the mapping stays 1:1 with the wire format.
//
// Zero-valued fields are the common case (no free quota granted for
// that family). The refresher writes every field on every tick — a
// dropped-to-zero value means the plan no longer grants that quota,
// not a "keep the old value" signal.
type FreeQuotaCounters struct {
	FaceSwapCredits          float64
	SoulCredits              float64
	CharacterSwapCredits     float64
	QwenCameraControlCredits float64
	Wan25VideoCredits        float64
	Text2KeyframesCredits    float64
	Veo3FastGenerationsCount float64
}

// UnlimActivation is one row of the account_unlim_activations table.
// Populated by the refresher from GET /workspaces/unlim-activations
// per active account and consumed by the load-balance router when the
// operator opts into `load_balance.prefer_unlim`.
//
// BundleType is the operator-facing bundle (e.g. "nano_banana_2_2k")
// and JobSetType is the unlim endpoint that bundle unlocks (e.g.
// "nano_banana_pro_unlimited"). PickAndLock joins on JobSetType.
type UnlimActivation struct {
	// ID is the activation's server-side id, needed to POST a claim
	// (/workspaces/unlim-activations/{id}). Not persisted — the store
	// keys by (account_id, bundle_type) and never reads ID back.
	ID          string
	BundleType  string
	JobSetType  string
	Resolutions []string
	ExpiresAt   time.Time // zero == no expiry
	ActivatedAt time.Time
	// IsClaimed is false for bundles the platform granted but the user
	// has not activated yet. The claimer POSTs a claim for these; the
	// store does not persist this flag.
	IsClaimed bool
}

// AvailableSlots returns how many more concurrent jobs this account can accept
// before hitting the upstream limit of 6.
func (a *Account) AvailableSlots() int {
	const upstreamLimit = 6
	slots := upstreamLimit - int(a.InFlightJobs)
	if slots < 0 {
		return 0
	}
	return slots
}

// CanAfford reports whether the account has enough subscription balance to run
// a job with the given estimated cost (in credits * 100 units), with a 20% buffer.
func (a *Account) CanAfford(estCostHundredths int64) bool {
	return a.SubscriptionBalance >= (estCostHundredths*120)/100
}
