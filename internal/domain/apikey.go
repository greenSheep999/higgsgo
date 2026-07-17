package domain

import "time"

// APIKeyStatus values recorded in api_keys.status.
//
//   - "active":  usable by /v1/* callers.
//   - "paused":  temporarily disabled by an operator; the row keeps its
//     usage / audit / group bindings intact and can be flipped back to
//     "active" via /admin/keys/{id}/resume.
//   - "revoked": terminal state. The row is retained for audit, but the
//     key can never be reused — including via resume.
const (
	APIKeyStatusActive  = "active"
	APIKeyStatusPaused  = "paused"
	APIKeyStatusRevoked = "revoked"
)

// APIKey is an authentication credential issued by higgsgo. Standalone
// keys are minted via /admin/keys; CPA-mode keys are minted via
// /internal/register and carry a non-empty CPAPartnerID so all
// /internal/* routes can locate the row set for a partner without
// walking every row in api_keys.
type APIKey struct {
	ID           string
	KeyHash      string // SHA-256 hex digest; the plaintext is shown once at creation
	Name         string
	CreatedBy    string
	CPAPartnerID string // when non-empty, this key is scoped to a CPA partner
	GroupID      string // when non-empty, this key is bound 1:1 to this pool group
	Status       string // one of APIKeyStatus* constants above

	// Quota tracking (credits * 100).
	MonthlyQuota int64 // 0 means unlimited
	MonthlyUsed  int64

	// Optional markup on top of actual upstream cost.
	// If MarkupPct = 1.5, the caller is charged 1.5x the actual credits.
	MarkupPct float64

	CreatedAt  time.Time
	LastUsedAt time.Time
}

// HasBudget reports whether the API key has quota left for a job with the
// given charged cost. Returns true when MonthlyQuota is 0 (unlimited).
func (k *APIKey) HasBudget(chargedHundredths int64) bool {
	if k.MonthlyQuota == 0 {
		return true
	}
	return k.MonthlyUsed+chargedHundredths <= k.MonthlyQuota
}
