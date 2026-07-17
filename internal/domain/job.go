package domain

import "time"

// JobStatus is the lifecycle state of a proxied generation request.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "in_progress"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
	JobRefunded  JobStatus = "refunded"
	JobTimeout   JobStatus = "timeout"
)

// ErrorType classifies why a job did not complete successfully.
// Used for metrics labels and for deciding whether to retry / switch accounts.
type ErrorType string

const (
	ErrNone      ErrorType = ""
	ErrBody      ErrorType = "body_error"    // 422 pydantic validation
	ErrGate      ErrorType = "gate"          // 403 plan/permission gate
	ErrRateLimit ErrorType = "rate_limit"    // 429
	ErrUpstream  ErrorType = "upstream_fail" // create OK, job failed downstream
	ErrTimeout   ErrorType = "poll_timeout"  // create OK, never reached terminal state
	ErrNetwork   ErrorType = "network"       // TCP / TLS / socks5 error
	ErrIPCheck   ErrorType = "ip_check"      // 400 IP check not finished for input media
	ErrUnknown   ErrorType = "unknown"
)

// Job is one end-to-end proxied generation.
// Persisted to the jobs table by JobStore.
type Job struct {
	ID           string // our own UUID, returned to the API caller
	APIKeyID     string // caller (standalone mode); empty in CPA mode
	CPAPartnerID string // caller (CPA mode); empty in standalone mode
	GroupID      string // pool group used
	AccountID    string // account picked from the pool

	// Request.
	ModelAlias      string // what the user asked for
	JST             string // resolved job_set_type
	Endpoint        string
	RequestBodyJSON string
	RequestTS       time.Time

	// Upstream reference.
	UpstreamJobID string
	UpstreamCost  int64 // cost field returned by /jobs create
	ResultURL     string

	// Outcome.
	Status      JobStatus
	ErrorType   ErrorType
	ErrorDetail string
	FinishedAt  time.Time
	LatencyMS   int64
	PollCount   int

	// Accounting (see internal/core/metering).
	ActualCreditsHundredths  int64 // computed from subscription_balance delta
	ChargedCreditsHundredths int64 // billed to APIKey / CPA partner
	Refunded                 bool

	// PreBalanceH is the caller account's subscription_balance snapshot
	// captured immediately before the upstream job was created, expressed
	// in credits*100 units. Persisted so the async pollworker can compute
	// exact actual-credits consumed via (PreBalanceH - post.SubscriptionBalance)
	// the same way the sync proxy path does; without this the pollworker
	// falls back to UpstreamCost, which drifts from truth for partial-refund
	// cases. Zero means "unknown" and the metering Recorder falls back.
	PreBalanceH int64

	// CallbackURL, when non-empty, is the caller-supplied HTTP endpoint that
	// receives a signed webhook when the job reaches a terminal state.
	// Populated at create time from the /v1 request body's `callback_url`
	// field and consumed by both the sync proxy path and the pollworker.
	CallbackURL string
}
