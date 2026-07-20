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

	// ResetAllInFlight zeroes in_flight_jobs across every row in one UPDATE
	// and returns the number of rows that were previously non-zero. Called
	// once at process boot to clear any counter leaks from a prior crash
	// or panic between PickAndLock and Unlock (see docs/ROADMAP.md P0-2).
	// Safe to call on a healthy DB: rows already at zero are unaffected.
	ResetAllInFlight(ctx context.Context) (int, error)

	MarkStatus(ctx context.Context, id string, status domain.AccountStatus, reason string) error

	// MarkThrottled parks the account in status=throttled and stamps
	// throttled_until with the caller-supplied deadline. The pool router
	// skips throttled rows until that deadline; the Recoverer goroutine
	// flips them back to active once it passes. reason is persisted to
	// accounts.status_reason so the admin surface can render "why". Set
	// by the failover controller (mechanism ②) — do not use this path for
	// operator-initiated pauses (that's StatusSuspended via MarkStatus).
	MarkThrottled(ctx context.Context, id string, until time.Time, reason string) error

	// RecoverThrottled bulk-flips status=throttled AND throttled_until<=now
	// back to status=active in a single UPDATE. Returns the number of rows
	// affected so the caller can log a summary. Invoked periodically by
	// core/failover.Recoverer.
	RecoverThrottled(ctx context.Context) (int, error)

	// IncrFailStreak atomically increments fail_streak and updates
	// last_failed_at, returning the new streak value so the caller can
	// judge whether the consecutive-fail limit was hit. Preferred over
	// MarkStatus for tick-only writes because it does not flip status
	// and does not overwrite status_reason.
	IncrFailStreak(ctx context.Context, id string) (int, error)

	// ResetFailStreak zeroes fail_streak. Called on every successful
	// upstream outcome (mechanism ①'s recovery edge) so a transient
	// hiccup does not accumulate across weeks of healthy traffic.
	ResetFailStreak(ctx context.Context, id string) error

	// PickAndLock atomically selects an eligible account and increments its
	// in_flight counter. Returns the selected account and an opaque lock
	// token that Unlock must be called with when the caller is done.
	//
	// Implementations should sort by params.RouteStrategy (round_robin by
	// default) and observe params.GroupID membership when non-empty.
	PickAndLock(ctx context.Context, params PickParams) (*domain.Account, string, error)

	// Unlock releases the in_flight increment claimed by PickAndLock.
	Unlock(ctx context.Context, id string, lockToken string) error

	// UpdateFreeQuota overwrites the seven per-family free-quota counters
	// on the given account row. Called by the refresher on every tick
	// after GET /user. Values that ship as zero from upstream still land
	// as zero on disk (the plan no longer grants that quota); the
	// refresher is the sole writer so there is no partial-update path.
	UpdateFreeQuota(ctx context.Context, id string, q domain.FreeQuotaCounters) error

	// ListUnlimActivations returns every account_unlim_activations row
	// for the given account. Ordered by bundle_type ASC for determinism
	// so a caller diffing two calls sees a stable sequence. Callers
	// interested in "which accounts hold this bundle" should use the
	// join in PickAndLock's ORDER BY instead — this method is intended
	// for the admin surface and refresher diff logic.
	ListUnlimActivations(ctx context.Context, accountID string) ([]domain.UnlimActivation, error)

	// ReplaceUnlimActivations swaps the full activations set for the
	// given account. Used by the refresher: upstream returns the
	// authoritative list on every /workspaces/unlim-activations call so
	// a "delete + insert" replace is the correct model (an activation
	// that vanished server-side must vanish locally too). The write
	// runs inside a transaction so concurrent PickAndLock calls see
	// either the old set or the new set, never a partial mix.
	ReplaceUnlimActivations(ctx context.Context, accountID string, activations []domain.UnlimActivation) error
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
	MinPlan           domain.PlanType
	RequiresPaid      bool
	RequiresUltra     bool
	RequiresUnlim     bool

	// Group scoping.
	GroupID string // empty means "default" group

	// Routing.
	RouteStrategy domain.RouteStrategy // empty means group-defined default

	// Group-level concurrency cap. When > 0 and GroupID != "", PickAndLock
	// refuses to pick if SUM(in_flight_jobs) across the group's members
	// has already reached this value. Enforced inside the transaction so
	// two concurrent picks cannot both slip past the check.
	//
	// Zero disables the check (unlimited group aggregate, subject only to
	// each account's per-row cap). Semantic: "at most N jobs alive across
	// all accounts in this group at once".
	MaxGroupInFlight int

	// Per-account concurrency ceiling. When > 0, PickAndLock refuses to
	// pick an account whose in_flight_jobs >= this value. Zero falls back
	// to the historical hardcoded cap of 5 so behavior stays stable for
	// callers that don't resolve group settings.
	//
	// Sourced from Group.MaxConcurrentPerAccount today; will feed from
	// resolver.Resolve() once ROADMAP P1-4 lands.
	MaxConcurrentPerAccount int

	// LoadBalance carries the operator-configurable knobs for the
	// load_balance route strategy. Populated by the proxy service from
	// the SettingsStore before every Pick; a zero value means "use the
	// hardcoded defaults" (tier-aware ordering ON, jitter ON, headroom
	// 120%, richer/unlim/free-quota preferences OFF). Consumed only when
	// RouteStrategy == "round_robin" (the load_balance mapping); other
	// strategies ignore this field.
	LoadBalance LoadBalanceOpts

	// UnlimJobSetType is the model's unlim endpoint identifier (e.g.
	// "nano_banana_pro_unlimited"). Populated by the proxy service from
	// ModelSpec.UnlimJobSetType. Consumed only when the operator toggled
	// `load_balance.prefer_unlim` on: PickAndLock sorts accounts that
	// hold a matching row in account_unlim_activations first. Empty
	// disables the preference regardless of the flag.
	UnlimJobSetType string

	// FreeQuotaField is the accounts column name that tracks this
	// model's free-quota counter (e.g. "face_swap_credits"). Populated
	// by the proxy service from ModelSpec.FreeQuotaField. Consumed only
	// when the operator toggled `load_balance.prefer_free_quota` on:
	// PickAndLock sorts accounts whose named column is > 0 first. Empty
	// disables the preference regardless of the flag.
	//
	// The column dispatch happens via a static CASE inside PickAndLock
	// (rather than fmt.Sprintf) so a mis-configured spec cannot inject
	// SQL — only names PickAndLock explicitly enumerates take effect.
	FreeQuotaField string
}

// LoadBalanceOpts is the operator-editable subset of the load_balance
// route strategy's internal ordering knobs. Persisted per-key in
// system_settings under load_balance.* keys and read on every pick.
//
// Zero-value semantics: an all-zero struct is treated as "no operator
// config present" and PickAndLock falls back to the hardcoded defaults
// (tier-aware ON, jitter ON, headroom 120%, other preferences OFF).
// Callers should either populate all fields or leave the struct at zero
// — mixing (e.g. setting TierAware=false but leaving Jitter=false when
// the operator wanted Jitter=true) is not a supported combination.
type LoadBalanceOpts struct {
	// Populated marks whether this struct carries operator-supplied
	// values. When false, PickAndLock ignores every other field and
	// applies the hardcoded defaults. This lets callers pass a zero
	// LoadBalanceOpts through the params without the store having to
	// distinguish "operator turned everything off" from "no config".
	Populated bool

	// TierAware, when true, keeps the cheap-first plan-tier CASE in the
	// ORDER BY clause. When false, PickAndLock skips the tier ranking
	// so accounts of any plan tier are considered equally.
	TierAware bool

	// PreferUnlim, when true, prefers accounts that have activated the
	// model's unlim bundle. Requires the account_unlim_activations
	// table + model.unlim_job_set_type wiring; when those are absent
	// this flag is a no-op (see TODO in AccountStore.PickAndLock).
	PreferUnlim bool

	// PreferFreeQuota, when true, prefers accounts whose free-quota
	// counter for the model's family is > 0. Requires the account-side
	// free-quota columns synced by the refresher; when absent this
	// flag is a no-op (see TODO in AccountStore.PickAndLock).
	PreferFreeQuota bool

	// PreferRicher, when true, adds subscription_balance DESC to the
	// ORDER BY tail so the account with the deepest wallet wins ties
	// within the same tier. Useful when operators want to burn the
	// richest account down first rather than spreading evenly.
	PreferRicher bool

	// BalanceHeadroomPct is the percentage of EstCostHundredths the
	// account's subscription_balance must exceed to qualify. 120 means
	// "balance >= cost * 1.2" (the historical default). 100 means "no
	// headroom, balance >= cost exactly"; higher values reserve more
	// buffer against mid-job cost drift. Valid range: 100..500.
	// A zero value falls back to 120.
	BalanceHeadroomPct int

	// Jitter, when true, appends RANDOM() as the final ORDER BY
	// tiebreaker so concurrent picks with identical primary keys land
	// on different rows probabilistically. When false, PickAndLock is
	// fully deterministic — useful for testing.
	Jitter bool
}

// JobStore persists proxied job records.
type JobStore interface {
	Create(ctx context.Context, j *domain.Job) error
	UpdateStatus(ctx context.Context, id string, status domain.JobStatus, meta JobMeta) error
	// TryMarkTerminal is the compare-and-swap terminal-transition writer
	// added by F1. It atomically moves a job into `to` iff the row's
	// current status is in `from`, and returns won=true only for the
	// call that actually performed the write. Callers use the flag to
	// gate metering, webhook fire, and in-flight release so a concurrent
	// sync path + pollworker do not both run those side effects.
	//
	// A won=false result is a race-lost signal, NOT an error. Real SQL
	// failures still surface via err. The winner's meta lands on the
	// row; a loser's stale snapshot cannot overwrite it. `from` must be
	// non-empty and `to` must be a terminal status (completed / failed /
	// refunded / timeout) — implementations should refuse other inputs
	// so this method is not used as an unguarded UpdateStatus stand-in.
	TryMarkTerminal(ctx context.Context, id string, from []domain.JobStatus, to domain.JobStatus, meta JobMeta) (bool, error)
	Get(ctx context.Context, id string) (*domain.Job, error)
	ListPending(ctx context.Context) ([]domain.Job, error)
	// ListByAPIKey returns jobs authored by apiKeyID, newest first
	// (ORDER BY request_ts DESC). Empty apiKeyID is treated as "match
	// no rows" so a misconfigured caller cannot accidentally dump the
	// full jobs table.
	ListByAPIKey(ctx context.Context, apiKeyID string, filter JobFilter) ([]domain.Job, error)
	// ListAll returns jobs across every api_key_id / account_id, newest
	// first (ORDER BY request_ts DESC). Intended for the /admin/jobs
	// operator surface — the public /v1/jobs path must keep using
	// ListByAPIKey so callers cannot peek at other callers' rows.
	//
	// Every JobFilter field is optional. When all filters are empty the
	// call returns the newest Limit rows across the whole table.
	ListAll(ctx context.Context, filter JobFilter) ([]domain.Job, error)

	// Purge deletes finished jobs whose finished_at is strictly older than
	// olderThan and whose status is in statuses. It is meant for periodic
	// housekeeping — usage_events retains the accounting rows, so deleting
	// the jobs row does not lose billing state.
	//
	// Callers should restrict statuses to terminal states (completed /
	// failed / refunded / timeout); passing an in-flight status like
	// pending or in_progress would delete jobs the pollworker still owns.
	// An empty statuses slice is a no-op: the method returns (0, nil)
	// so a mis-configured caller cannot accidentally wipe every finished
	// job by omitting the filter.
	//
	// Implementations must run the delete inside a transaction and return
	// the number of rows removed.
	Purge(ctx context.Context, olderThan time.Time, statuses []domain.JobStatus) (int, error)
}

// JobFilter narrows a JobStore.ListByAPIKey / JobStore.ListAll call.
//
// All fields are optional; zero values mean "no filter". Limit defaults to
// 100 and is capped at 500 by the store implementation to keep a single
// call from paging the whole table.
//
// AccountID / APIKeyID / GroupID / ModelAlias are ignored by ListByAPIKey
// (which always scopes to its explicit apiKeyID arg) and only apply to
// ListAll where every dimension is optional.
type JobFilter struct {
	Status     domain.JobStatus // empty means any status
	Since      time.Time        // inclusive lower bound on request_ts
	Until      time.Time        // exclusive upper bound on request_ts
	AccountID  string           // ListAll only; empty means any account
	APIKeyID   string           // ListAll only; empty means any api key
	GroupID    string           // ListAll only; empty means any group
	ModelAlias string           // ListAll only; empty means any model alias
	Limit      int
	Offset     int
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

// APIKeyStore manages standalone-mode and CPA-mode API keys.
type APIKeyStore interface {
	Get(ctx context.Context, id string) (*domain.APIKey, error)
	GetByHash(ctx context.Context, keyHash string) (*domain.APIKey, error)
	Create(ctx context.Context, k *domain.APIKey) error
	Revoke(ctx context.Context, id string) error
	IncrementUsage(ctx context.Context, id string, chargedHundredths int64) error
	List(ctx context.Context) ([]domain.APIKey, error)
	// ListByCPAPartner returns every api_keys row whose CPAPartnerID
	// equals partnerID, newest first. An empty partnerID must return
	// an empty slice so a misconfigured caller cannot enumerate every
	// standalone key. Callers apply their own status filtering.
	ListByCPAPartner(ctx context.Context, partnerID string) ([]domain.APIKey, error)

	// Rotate replaces the key_hash for the given id with a freshly
	// minted secret and returns the new plaintext. The caller is
	// expected to expose the plaintext to the user exactly once (like
	// the initial Create path). All other columns — name, quota,
	// markup, group bindings, CPA partner id — are preserved so a
	// rotation never breaks existing routing/accounting state.
	//
	// Returns domain.ErrAPIKeyNotFound when id does not exist.
	Rotate(ctx context.Context, id string) (newPlaintext string, err error)

	// Pause flips status to "paused". A paused key is rejected by the
	// /v1/* auth middleware but is not soft-deleted: usage counters,
	// audit trail, and group bindings all stay in place so an operator
	// can flip the row back to "active" via Resume.
	//
	// Only the "active" -> "paused" transition is legal. Calling Pause
	// on a revoked key returns domain.ErrAPIKeyRevoked; calling it on
	// an already-paused key is a no-op that returns nil.
	// Returns domain.ErrAPIKeyNotFound when id does not exist.
	Pause(ctx context.Context, id string) error

	// Resume flips status back to "active". Only the "paused" ->
	// "active" transition is legal. Calling Resume on a revoked key
	// returns domain.ErrAPIKeyRevoked without touching the row (revoked
	// is terminal). Calling it on an already-active key is a no-op
	// that returns nil.
	// Returns domain.ErrAPIKeyNotFound when id does not exist.
	Resume(ctx context.Context, id string) error

	// ResetMonthlyUsage zeros the monthly_used counter on the given
	// row. Intended to be called by the month-boundary ticker (not
	// wired yet) or manually by an operator via the admin API on
	// credit-refund / complaint flows. Does not touch monthly_quota so
	// the caller keeps their configured cap.
	// Returns domain.ErrAPIKeyNotFound when id does not exist.
	ResetMonthlyUsage(ctx context.Context, id string) error

	// UpdatePlaygroundScope replaces the playground_scope column on the
	// given row. The scope controls whether the key can invoke the
	// interactive /v1/playground/* surface used by the WebUI (see
	// migration 009). Unknown values are normalised to
	// domain.PlaygroundScopeNone by the implementation so a malformed
	// operator write cannot silently open access.
	// Returns domain.ErrAPIKeyNotFound when id does not exist.
	UpdatePlaygroundScope(ctx context.Context, id string, scope domain.PlaygroundScope) error

	// UpdateMeta patches the mutable metadata columns of an api_keys
	// row. Each pointer is optional; a nil pointer means "leave the
	// column alone". Callers must not use this path to touch key_hash,
	// status, monthly_used, or CPA/group binding fields — those flow
	// through their own methods so audit trails and lifecycle
	// invariants stay clean. Returns domain.ErrAPIKeyNotFound when id
	// does not exist.
	UpdateMeta(ctx context.Context, id string, patch APIKeyMetaPatch) error
}

// APIKeyMetaPatch is the partial-update body accepted by
// APIKeyStore.UpdateMeta. All fields are pointers so the caller can
// send a subset and leave the rest untouched.
type APIKeyMetaPatch struct {
	Name         *string
	MonthlyQuota *int64
	MarkupPct    *float64
}

// GroupStore manages account pool groups.
// GroupMember is one row in an account-to-group association, exposed via
// ListMembersWithPriority so admins can see and edit the per-group
// priority in the UI without needing separate queries.
type GroupMember struct {
	AccountID string
	Priority  int
}

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
	// ListMembersWithPriority returns members paired with their per-group
	// priority. Used by the admin UI to show and edit the priority column
	// (backend already reads it via PickAndLock when route_strategy =
	// "priority"). Ordered priority DESC, account_id ASC for determinism.
	ListMembersWithPriority(ctx context.Context, groupID string) ([]GroupMember, error)

	// Bindings.
	BindAPIKey(ctx context.Context, apiKeyID, groupID string) error
	UnbindAPIKey(ctx context.Context, apiKeyID, groupID string) error
	ListGroupsForAPIKey(ctx context.Context, apiKeyID string) ([]domain.Group, error)
	ListAPIKeys(ctx context.Context, groupID string) ([]string, error)

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
	// List returns rows matching the filter, newest-first
	// (created_at DESC / id DESC). Empty Status matches all. Limit
	// defaults to 50 and is capped at 200 by the adapter; a zero /
	// negative value falls back to the default. Since=zero disables
	// the time filter. Offset supports paging for the admin UI. See
	// docs/ROADMAP.md §5.4.
	List(ctx context.Context, filter RegistrationFilter) ([]Registration, error)
	// ResetToPending flips a failed / terminal row back to pending
	// so the worker re-picks it on the next tick. Used by
	// admin.Retry. Attempts is preserved so operators can see how
	// many times a row has been tried. Returns
	// domain.ErrRegistrationNotFound on unknown id. Callers should
	// only invoke this on rows currently in a non-terminal state
	// that they know about — the store does not restrict source
	// state so admins can force-retry from any state if they
	// choose.
	ResetToPending(ctx context.Context, id int64) error
}

// RegistrationFilter is declared in ports/registrar.go; reused here.

// Registration is a pending or completed account registration attempt.
//
// MailboxClientID / MailboxRefreshToken are the Microsoft Graph OAuth2
// credentials the Node driver uses to fetch this mailbox's OTP email.
// Both are populated on every password-flow row from the bulk import
// (line format: email----password----client_id----refresh_token).
// OAuth flows leave them empty. See migration 017 and ROADMAP §5.4.
type Registration struct {
	ID                  int64
	Email               string
	Password            string
	OAuthSource         string
	RefreshToken        string // legacy — kept for the OAuth path; unrelated to MailboxRefreshToken
	ProxyURL            string
	Status              string
	Attempts            int
	LastError           string
	AccountID           string // filled on success
	CreatedAt           time.Time
	FinishedAt          time.Time
	MailboxClientID     string
	MailboxRefreshToken string
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

// AuditStore persists admin write-op audit rows. Populated by the audit
// middleware (see internal/api/middleware/audit.go) on every mutating
// /admin/* request; read back by the /admin/audit list endpoint.
//
// Implementations are append-only: there is no Update or Delete surface —
// rows are inserted with an idgen id and only ever read via List. The
// middleware calls Insert in a background goroutine so a slow database
// cannot block the API response.
type AuditStore interface {
	Insert(ctx context.Context, e *domain.AuditEvent) error
	List(ctx context.Context, filter AuditFilter) ([]domain.AuditEvent, error)
}

// AuditFilter narrows an AuditStore.List call. All fields are optional;
// zero values mean "no filter". Limit defaults to 100 and is capped at
// 500 by the store implementation so a single call cannot dump the whole
// table.
type AuditFilter struct {
	Since        time.Time
	Until        time.Time
	Actor        string
	ResourceType string
	ResourceID   string
	Method       string
	Limit        int
	Offset       int
}

// ModelHealthStore records the outcome of periodic recheck runs.
type ModelHealthStore interface {
	Insert(ctx context.Context, jst string, checkedAt time.Time, verdict domain.JobStatus, httpStatus int, cost int64, pollSec int) error
	Latest(ctx context.Context, jst string) (*ModelHealthRow, error)
	// List returns every model_health row across all jst values, newest
	// first (ORDER BY checked_at DESC). No pagination: the table is
	// bounded by the model catalog size (~130 jsts × recent history),
	// which is small enough for admin surfaces to consume in one shot.
	List(ctx context.Context) ([]ModelHealthRow, error)
	// UptimeByJST computes the uptime percentage for every jst that has
	// at least one probe within the window [since, now). Returns a map
	// of jst -> uptime percentage (0.0–100.0). A probe is counted as
	// "ok" when its verdict equals "completed". The table is small
	// enough for a single aggregate query.
	UptimeByJST(ctx context.Context, since time.Time) (map[string]float64, error)
	// SlotsByJST buckets probes for one JST into `count` fixed-width
	// slots so the WebUI can render a real per-slot uptime bar. slotSec
	// controls the bucket width; typical values are 3600 (1h) or 86400
	// (1d) matching the frontend's slot counts (12 or 48). Slots are
	// returned oldest-first with total=0 for windows that saw no
	// probes. See docs/ROADMAP.md P3-13.
	SlotsByJST(ctx context.Context, jst string, count int, slotSec int) ([]HealthSlot, error)
}

// HealthSlot is one bucket of probe outcomes for a single JST over a
// fixed time window. Total counts every probe row that landed in
// [Time, Time+SlotSec); Passed counts those whose verdict was
// "completed". The frontend renders green when Passed/Total >= 1.0,
// yellow when >= 0.8, red otherwise; Total == 0 is muted "no data".
type HealthSlot struct {
	Time   time.Time
	Total  int
	Passed int
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

// FailoverEventStore persists the raw failover-controller events used
// for the sliding-window judge / evict counters and the admin audit
// trail. Every write is a single INSERT and every read a single COUNT
// / SELECT, so a naive SQLite implementation is thread-safe without
// extra locking (the driver's connection pool handles concurrency).
type FailoverEventStore interface {
	Insert(ctx context.Context, accountID string, kind FailoverEventKind, reason string, httpStatus int) error
	Count(ctx context.Context, accountID string, kind FailoverEventKind, windowSec int) (int, error)
	CountRecentDisables(ctx context.Context, windowSec int) (int, error)
	List(ctx context.Context, accountID string, limit int) ([]FailoverEventRow, error)
	DeleteForAccount(ctx context.Context, accountID string) error
}

// FailoverEventKind enumerates the persisted event kinds.
type FailoverEventKind string

const (
	FailoverEventFailure   FailoverEventKind = "failure"
	FailoverEventThrottle  FailoverEventKind = "throttle"
	FailoverEventBlacklist FailoverEventKind = "blacklist"
)

// FailoverEventRow mirrors one account_failover_events row.
type FailoverEventRow struct {
	ID         int64
	AccountID  string
	Kind       FailoverEventKind
	Reason     string
	HTTPStatus int
	CreatedAt  time.Time
}

// FailoverOverridesStore persists per-account overrides of the global
// failover tunables.
type FailoverOverridesStore interface {
	Get(ctx context.Context, accountID string) (*FailoverOverride, error)
	Upsert(ctx context.Context, o *FailoverOverride) error
	Delete(ctx context.Context, accountID string) error
}

// FailoverOverride mirrors one account_failover_overrides row. A
// pointer field means "explicitly overridden"; nil means "inherit
// global default".
type FailoverOverride struct {
	AccountID      string
	Enabled        *bool
	FailLimit      *int
	JudgeWindowSec *int
	JudgeCount     *int
	CooldownSec    *int
	EvictWindowSec *int
	EvictCount     *int
	UpdatedAt      time.Time
}

// SettingsStore persists operator-editable configuration overrides.
// Values that were originally loaded from configs/*.toml can be
// mutated at runtime (e.g. rotating the admin bearer from the WebUI)
// and the override survives restarts because it lives in the DB.
//
// The store is intentionally minimal — a plain key/value surface —
// so new operator-editable settings can land without another port
// change. The single row schema mirrors the system_settings table
// added in migration 014.
type SettingsStore interface {
	// Get returns the raw string value for key. Returns
	// domain.ErrSettingNotFound when the row is absent so callers can
	// fall back to the TOML-loaded default without inspecting a
	// second error type.
	Get(ctx context.Context, key string) (string, error)

	// Set writes value under key, replacing any existing row. The
	// updated_at column is refreshed to the current wall-clock time
	// by the store.
	Set(ctx context.Context, key, value string) error

	// UpdatedAt returns the wall-clock time value under key was last
	// written. Returns domain.ErrSettingNotFound when the row is
	// absent. Kept separate from Get so callers that just want the
	// value do not pay for a second column parse.
	UpdatedAt(ctx context.Context, key string) (time.Time, error)
}

// ModelOverrideStore persists operator overrides that layer on top of
// the static jsonstatic ModelSpec catalog. The registry consults this
// store on Reload() and after every /admin/models/reload to rebuild
// the in-memory merged map, and admin writes call Upsert / Delete
// one row at a time.
type ModelOverrideStore interface {
	// Get returns the override for alias, or (nil, nil) when no row
	// exists. A nil result must be treated as "no override — spec
	// defaults apply". Callers should not need to inspect
	// domain.ErrModelOverrideNotFound for the common absence case.
	Get(ctx context.Context, alias string) (*domain.ModelOverride, error)

	// Upsert writes (or replaces) the override for o.Alias. Pointer
	// fields land as NULL when nil so the registry's merge helper
	// falls back to spec defaults on read.
	Upsert(ctx context.Context, o *domain.ModelOverride) error

	// Delete removes the override row for alias. Missing rows are a
	// no-op so callers can idempotently reset.
	Delete(ctx context.Context, alias string) error

	// List returns every override row, newest updated first. Used by
	// the admin surface to render the overrides table and by the
	// registry to rebuild its merged map on Reload().
	List(ctx context.Context) ([]domain.ModelOverride, error)
}
