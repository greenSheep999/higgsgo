package domain

import "time"

// UsageEvent is a single accounting record emitted when a job reaches a terminal
// state. Persisted to the usage_events table and rolled up into usage_daily_agg
// by the metering ticker.
//
// One event may attribute to either an APIKey (standalone mode) or a CPA
// partner (CPA mode) — never both.
type UsageEvent struct {
	ID string
	TS time.Time

	// Who.
	APIKeyID     string // standalone
	CPAPartnerID string // CPA
	CPAUserID    string // CPA-side end user, opaque to higgsgo
	GroupID      string
	AccountID    string

	// What.
	ModelAlias string
	JST        string
	MediaType  string // "image" | "video" | "audio"

	// Cost.
	UpstreamCost             int64   // cost field returned by /jobs create
	ActualCreditsHundredths  int64   // computed from balance delta
	ChargedCreditsHundredths int64   // what we bill the caller
	MarkupPct                float64 // 1.0 = no markup

	// Outcome.
	Status    JobStatus
	LatencyMS int64
	PollCount int
	ErrorType ErrorType

	// References.
	HiggsgoJobID  string
	UpstreamJobID string
	ResultURL     string

	// Denormalized for cheap group-by.
	BillingMonth string // "2026-07"
	BillingDay   string // "2026-07-17"
}
