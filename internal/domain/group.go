package domain

import "time"

// RouteStrategy picks an account inside a group.
type RouteStrategy string

const (
	// RouteRoundRobin picks the least-recently-used account.
	RouteRoundRobin RouteStrategy = "round_robin"

	// RouteLeastUsed picks the account with the least accumulated month-to-date credits.
	RouteLeastUsed RouteStrategy = "least_used"

	// RouteCheapestFirst picks the lowest-tier eligible account first,
	// preserving high-tier account budgets for models that require them.
	RouteCheapestFirst RouteStrategy = "cheapest_first"

	// RouteMostCreditsFirst picks the account with the largest remaining
	// subscription balance. Useful for high-cost models.
	RouteMostCreditsFirst RouteStrategy = "most_credits_first"

	// RoutePriority orders candidates by accounts.priority DESC. Used
	// when the operator wants deterministic routing to preferred
	// accounts before falling back to the rest of the pool. Pairs with
	// the sidebar "priority" mode via the routing_strategy_default
	// system setting.
	RoutePriority RouteStrategy = "priority"

	// RouteBestFit is a legacy alias absorbed into RouteRoundRobin as
	// of v0.5.5. Kept as a constant so older SQL rows / API payloads
	// containing the string still parse cleanly; the SQL builder
	// treats it the same as round_robin. Migration 019 rewrites any
	// stored value to 'round_robin' so this constant is only reachable
	// from in-flight API requests during a rolling upgrade.
	//
	// Deprecated: use RouteRoundRobin (which now performs the same
	// tier-aware ordering).
	RouteBestFit RouteStrategy = "best_fit"
)

// OwnerType describes who a group belongs to.
type OwnerType string

const (
	OwnerAPIKey   OwnerType = "apikey"
	OwnerCPA      OwnerType = "cpa_partner"
	OwnerInternal OwnerType = "internal"
)

// Group is a pool subdivision with its own quotas and routing policy.
type Group struct {
	ID          string
	Name        string
	Description string

	// Concurrency caps.
	MaxConcurrentJobs       int // aggregate across all member accounts
	MaxConcurrentPerAccount int // typically ≤5 to leave headroom below upstream's 6

	// Monthly budget in credits * 100.
	MonthlyCreditBudget int64
	MonthlyCreditUsed   int64

	// Model access.
	AllowedModelsRegex string // e.g., ".*"; empty means allow all
	BlockedModelsRegex string // e.g., "^veo3.*"; empty means block none

	// Routing.
	RouteStrategy RouteStrategy

	// Ownership.
	OwnerType OwnerType
	OwnerID   string

	Status    string
	CreatedAt time.Time
}
