package domain

import "time"

// ModelOverride is an operator-editable patch applied on top of a
// static ModelSpec (loaded from data/reference/verified-models.json)
// at Registry.Resolve() / List() time.
//
// Every tier field is a pointer: nil means "inherit spec", a set
// pointer means "explicit override". This mirrors the FailoverOverride
// convention so a partial write can null-out a single flag without
// echoing back the whole record.
//
// The primary key is Alias — a canonical higgsgo alias matching the
// static catalog. Overrides for a since-removed alias survive in the
// table but are quietly ignored on the merge side (no ghost specs).
type ModelOverride struct {
	Alias                string
	StarterLocked        *bool
	RequiresPaid         *bool
	RequiresUltra        *bool
	RequiresUnlim        *bool
	MinCreditsHundredths *int64
	// ExtraAliases carries additional names the model should be
	// advertised under on /v1/models. Empty/nil means no expansion.
	// The downstream aggregator (new-api) reads /v1/models and
	// registers each entry under `id` plus every string in
	// `extra_aliases`; higgsgo's own routing / auth / metering never
	// touch this field.
	ExtraAliases []string
	// Note is an operator memo, surfaced verbatim in the admin sheet.
	Note      string
	UpdatedAt time.Time
}
