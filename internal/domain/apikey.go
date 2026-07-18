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

// APIKeyKind classifies an api_keys row. `default` is the operator's
// own key (broad access; typically used from the admin console); it
// exists to give managers a key they can wield without having to
// pretend it's a partner-scoped project key. `project` is the
// externally-shared key issued to a downstream user / team; it gets
// tight defaults (playground=none, quota set) and is what group
// bindings usually attach to.
//
// Kept as string constants so future values (e.g. "internal",
// "experiment") can be added without a schema change beyond the
// enum documentation.
type APIKeyKind string

const (
	APIKeyKindDefault APIKeyKind = "default"
	APIKeyKindProject APIKeyKind = "project"
)

// PlaygroundScope gates a key's access to the /v1/playground/* interactive
// surface used by the WebUI. Defaults to PlaygroundScopeNone which fully
// disables the playground for the key, matching migration 009's default so
// pre-existing keys stay locked out until an operator opts them in.
type PlaygroundScope string

const (
	// PlaygroundScopeNone forbids /v1/playground/* entirely.
	PlaygroundScopeNone PlaygroundScope = "none"
	// PlaygroundScopeCheap allows models whose est_cost_hundredths is
	// at or below the cheap cap (500, i.e. 5 credits per generation).
	PlaygroundScopeCheap PlaygroundScope = "cheap"
	// PlaygroundScopeFull allows any registered model.
	PlaygroundScopeFull PlaygroundScope = "full"
)

// PlaygroundCheapCapHundredths is the est_cost cutoff (credits × 100) at
// or below which a "cheap"-scope key may invoke a model. Kept as a domain
// constant so both the middleware/handler code paths and any future
// tooling agree on the threshold without re-quoting the literal.
const PlaygroundCheapCapHundredths int64 = 500

// AllowsModel reports whether this scope permits invoking a model whose
// estimated cost is estCostHundredths (credits × 100). PlaygroundScopeNone
// always returns false; PlaygroundScopeFull always returns true; the
// PlaygroundScopeCheap gate compares against PlaygroundCheapCapHundredths.
// Any unrecognised value is treated as PlaygroundScopeNone (deny by
// default) so a mis-typed operator write cannot silently open access.
func (s PlaygroundScope) AllowsModel(estCostHundredths int64) bool {
	switch s {
	case PlaygroundScopeFull:
		return true
	case PlaygroundScopeCheap:
		return estCostHundredths <= PlaygroundCheapCapHundredths
	default:
		return false
	}
}

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

	// PlaygroundScope gates access to the /v1/playground/* interactive
	// surface used by the WebUI. Defaults to PlaygroundScopeNone.
	PlaygroundScope PlaygroundScope

	// Kind classifies the key as either an operator "default" key or a
	// downstream "project" key (see APIKeyKind). Defaults to "project"
	// when unset so historic rows don't accidentally get treated as
	// admin-level defaults.
	Kind APIKeyKind

	// KeyLast4 is the last 4 chars of the plaintext body. Kept so the
	// admin list can render a partial preview (`sk-hg-•••abc1`)
	// without revealing the full key. Populated at create / rotate;
	// empty for rows that pre-date migration 012.
	KeyLast4 string
}

// HasBudget reports whether the API key has quota left for a job with the
// given charged cost. Returns true when MonthlyQuota is 0 (unlimited).
func (k *APIKey) HasBudget(chargedHundredths int64) bool {
	if k.MonthlyQuota == 0 {
		return true
	}
	return k.MonthlyUsed+chargedHundredths <= k.MonthlyQuota
}
