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

// AccountStatus is the lifecycle state we track locally.
type AccountStatus string

const (
	StatusActive    AccountStatus = "active"
	StatusSuspended AccountStatus = "suspended"
	StatusExpired   AccountStatus = "expired"
	StatusBanned    AccountStatus = "banned"
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

	// Optional IP binding for models that require sticky IP (image2video_extend etc.).
	BoundProxyURL string

	// Priority is an operator-managed sort hint used by the pool router
	// to prefer some accounts over others (higher = picked first).
	// Default 0; range is [-1_000, 1_000] enforced at the handler layer.
	// See migration 010_accounts_priority.sql.
	Priority int

	RegisteredAt time.Time
	ImportedAt   time.Time
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
