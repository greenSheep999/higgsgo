package ports

import (
	"context"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// AccountStore persists and queries the account pool. Implementations back
// this with SQLite (default), Postgres, or an in-memory map for tests.
type AccountStore interface {
	Get(ctx context.Context, id string) (*domain.Account, error)
	List(ctx context.Context, filter AccountFilter) ([]domain.Account, error)
	Upsert(ctx context.Context, a *domain.Account) error
	UpdateBalance(ctx context.Context, id string, sub, credits, pkg int64) error
	// UpdateEntitlements refreshes the API-side permission flags observed via
	// GET /user (plan_type / has_unlim / has_flex_unlim / is_pro_veo3_available /
	// cohort). Called by the balance refresher ticker.
	UpdateEntitlements(ctx context.Context, id string, e EntitlementUpdate) error
	UpdateInFlight(ctx context.Context, id string, delta int) error
	MarkStatus(ctx context.Context, id string, status domain.AccountStatus, reason string) error

	// PickAndLock atomically selects an eligible account and increments its
	// in_flight counter. Returns the selected account and an opaque lock
	// token that Unlock must be called with when the caller is done.
	//
	// Implementations should sort by params.RouteStrategy (round_robin by
	// default) and observe params.GroupID membership when non-empty.
	PickAndLock(ctx context.Context, params PickParams) (*domain.Account, string, error)

	// Unlock releases the in_flight increment claimed by PickAndLock.
	Unlock(ctx context.Context, id string, lockToken string) error
}

// EntitlementUpdate carries the API-side permission fields refreshed from
// GET /user by the balance refresher ticker.
type EntitlementUpdate struct {
	PlanType           domain.PlanType
	HasUnlim           bool
	HasFlexUnlim       bool
	IsProVeo3Available bool
	Cohort             string
	TotalPlanCredits   int64 // credits × 100
	PlanEndsAt         time.Time
}

// AccountFilter narrows an AccountStore.List call.
type AccountFilter struct {
	Status     domain.AccountStatus // empty means any
	PlanType   domain.PlanType      // empty means any
	GroupID    string               // empty means any
	MinBalance int64                // 0 means any
	HasUnlim   *bool
	Since      time.Time
}

// PickParams describes what kind of account the pool caller needs.
type PickParams struct {
	// Model requirements.
	JST               string
	EstCostHundredths int64
	RequiresPaid      bool
	RequiresUltra     bool
	RequiresUnlim     bool

	// Group scoping.
	GroupID string // empty means "default" group

	// Routing.
	RouteStrategy domain.RouteStrategy // empty means group-defined default
}

// JobStore persists proxied job records.
type JobStore interface {
	Create(ctx context.Context, j *domain.Job) error
	UpdateStatus(ctx context.Context, id string, status domain.JobStatus, meta JobMeta) error
	Get(ctx context.Context, id string) (*domain.Job, error)
	ListPending(ctx context.Context) ([]domain.Job, error)
	// ListByAPIKey returns jobs authored by apiKeyID, newest first
	// (ORDER BY request_ts DESC). Empty apiKeyID is treated as "match
	// no rows" so a misconfigured caller cannot accidentally dump the
	// full jobs table.
	ListByAPIKey(ctx context.Context, apiKeyID string, filter JobFilter) ([]domain.Job, error)
}

// JobFilter narrows a JobStore.ListByAPIKey call.
//
// All fields are optional; zero values mean "no filter". Limit defaults to
// 100 and is capped at 500 by the store implementation to keep a single
// call from paging the whole table.
type JobFilter struct {
	Status domain.JobStatus // empty means any status
	Since  time.Time        // inclusive lower bound on request_ts
	Until  time.Time        // exclusive upper bound on request_ts
	Limit  int
	Offset int
}

// JobMeta carries the outcome fields written on status transitions.
type JobMeta struct {
	UpstreamJobID string
	ResultURL     string
	ErrorType     domain.ErrorType
	ErrorDetail   string
	LatencyMS     int64
	PollCount     int

	ActualCreditsHundredths  int64
	ChargedCreditsHundredths int64
	Refunded                 bool
}

// APIKeyStore manages standalone-mode API keys.
type APIKeyStore interface {
	Get(ctx context.Context, id string) (*domain.APIKey, error)
	GetByHash(ctx context.Context, keyHash string) (*domain.APIKey, error)
	Create(ctx context.Context, k *domain.APIKey) error
	Revoke(ctx context.Context, id string) error
	IncrementUsage(ctx context.Context, id string, chargedHundredths int64) error
	List(ctx context.Context) ([]domain.APIKey, error)
}

// GroupStore manages account pool groups.
type GroupStore interface {
	Get(ctx context.Context, id string) (*domain.Group, error)
	GetByName(ctx context.Context, name string) (*domain.Group, error)
	Create(ctx context.Context, g *domain.Group) error
	Update(ctx context.Context, g *domain.Group) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context) ([]domain.Group, error)

	// Membership.
	AddMember(ctx context.Context, groupID, accountID string, priority int) error
	RemoveMember(ctx context.Context, groupID, accountID string) error
	ListMembers(ctx context.Context, groupID string) ([]string, error)

	// Bindings.
	BindAPIKey(ctx context.Context, apiKeyID, groupID string) error
	UnbindAPIKey(ctx context.Context, apiKeyID, groupID string) error
	ListGroupsForAPIKey(ctx context.Context, apiKeyID string) ([]domain.Group, error)

	// Quota accounting.
	IncrementUsed(ctx context.Context, groupID string, deltaHundredths int64) error
	CurrentInFlight(ctx context.Context, groupID string) (int, error)
}

// UsageEventStore records the detail rows behind the metering system.
type UsageEventStore interface {
	Insert(ctx context.Context, e *domain.UsageEvent) error
	Query(ctx context.Context, q UsageQuery) ([]domain.UsageEvent, error)
	Aggregate(ctx context.Context, q UsageAggQuery) ([]UsageAggRow, error)
}

// UsageQuery selects usage_events rows.
type UsageQuery struct {
	Since        time.Time
	Until        time.Time
	APIKeyID     string
	CPAPartnerID string
	AccountID    string
	GroupID      string
	ModelAlias   string
	Status       string // domain.JobStatus value ("completed", "failed", ...)
	Limit        int
	Offset       int
}

// UsageAggQuery drives a group-by aggregation over usage_events.
type UsageAggQuery struct {
	Since   time.Time
	Until   time.Time
	GroupBy []string // subset of {api_key_id, cpa_partner_id, account_id, group_id, model_alias, billing_day}
	Filters UsageQuery
}

// UsageAggRow is one row of the aggregation result.
type UsageAggRow struct {
	Keys                     map[string]string // populated dimensions
	RequestCount             int64
	CompletedCount           int64
	FailedCount              int64
	RefundedCount            int64
	TotalCreditsHundredths   int64
	ChargedCreditsHundredths int64
	AvgLatencyMS             int64
}

// RegistrationStore persists the async registration queue.
type RegistrationStore interface {
	Enqueue(ctx context.Context, r *Registration) error
	NextPending(ctx context.Context) (*Registration, error)
	MarkRunning(ctx context.Context, id int64) error
	MarkCompleted(ctx context.Context, id int64, accountID string) error
	MarkFailed(ctx context.Context, id int64, errMsg string) error
	Get(ctx context.Context, id int64) (*Registration, error)
}

// Registration is a pending or completed account registration attempt.
type Registration struct {
	ID           int64
	Email        string
	Password     string
	OAuthSource  string
	RefreshToken string
	ProxyURL     string
	Status       string
	Attempts     int
	LastError    string
	AccountID    string // filled on success
	CreatedAt    time.Time
	FinishedAt   time.Time
}

// ProxyStore persists the IP proxy pool.
type ProxyStore interface {
	Insert(ctx context.Context, p *ProxyRow) error
	List(ctx context.Context, filter ProxyFilter) ([]ProxyRow, error)
	Update(ctx context.Context, p *ProxyRow) error
	MarkUsed(ctx context.Context, url string) error
	Delete(ctx context.Context, url string) error
}

// ProxyRow mirrors the proxy_pool table.
type ProxyRow struct {
	URL          string
	Provider     string
	Region       string
	BoundTo      string // account_id when sticky; empty when rotating
	Status       string
	LastHealthAt time.Time
	LastUsedAt   time.Time
	LatencyMS    int
}

// ProxyFilter narrows a ProxyStore.List query.
type ProxyFilter struct {
	Region  string
	Status  string
	Unbound bool // when true, only rows with BoundTo == ""
}

// ModelHealthStore records the outcome of periodic recheck runs.
type ModelHealthStore interface {
	Insert(ctx context.Context, jst string, checkedAt time.Time, verdict domain.JobStatus, httpStatus int, cost int64, pollSec int) error
	Latest(ctx context.Context, jst string) (*ModelHealthRow, error)
}

// ModelHealthRow is one row from the model_health table.
type ModelHealthRow struct {
	JST         string
	CheckedAt   time.Time
	Verdict     domain.JobStatus
	HTTPStatus  int
	Cost        int64
	PollTimeSec int
}
