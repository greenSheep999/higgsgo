package domain

import "time"

// APIKey is an authentication credential issued by higgsgo (standalone mode).
// In CPA mode the CPA platform issues its own keys and passes them through
// as CPAPartnerID; higgsgo does not manage those.
type APIKey struct {
	ID        string
	KeyHash   string // bcrypt hash; the plaintext is shown once at creation
	Name      string
	CreatedBy string
	Status    string // "active" | "revoked"

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
