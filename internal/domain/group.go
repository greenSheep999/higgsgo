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

	// RouteBestFit picks the account whose plan tier is closest to
	// (but not below) the model's MinPlan floor. Cheap models
	// (min_plan=starter) burn starter accounts first, reserving
	// pro / plus / ultra headroom for models that actually need it.
	// Falls back to jittered LRU inside the same tier so concurrent
	// picks on identical-plan accounts still spread out.
	//
	// Precedence inside the ORDER BY: tier_rank ASC (closest fit
	// wins) → last_used_at ASC (LRU) → in_flight_jobs ASC → RANDOM().
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
