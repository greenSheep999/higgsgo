// Package loadbalance owns the operator-editable knobs that steer the
// load_balance route strategy. Split out of internal/api/admin so both
// the admin handler (writer) and the proxy service (reader) can import
// it without the core layer depending on the api layer.
//
// The struct here is the persisted / wire shape; ports.LoadBalanceOpts
// is the shape PickAndLock consumes. ToOpts() bridges the two.
package loadbalance

import (
	"context"
	"strconv"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// system_settings row keys. Prefix keeps them grouped so a future
// GetPrefix helper on the store can hydrate the whole block in one
// round-trip.
const (
	// KeyTierAware — bool. Default true. When false, PickAndLock skips
	// the cheap-first plan-tier CASE in the load_balance ORDER BY so
	// tiers are considered equally.
	KeyTierAware = "load_balance.tier_aware"

	// KeyPreferUnlim — bool. Default false. Dormant today — see the
	// TODO in AccountStore.PickAndLock. When the
	// account_unlim_activations table + model.unlim_job_set_type
	// wiring land this flag makes bundle-holders sort first.
	KeyPreferUnlim = "load_balance.prefer_unlim"

	// KeyPreferFreeQuota — bool. Default false. Dormant today — see
	// the TODO in AccountStore.PickAndLock. When per-model free-quota
	// columns land on accounts, this flag makes accounts with a
	// non-zero counter sort first.
	KeyPreferFreeQuota = "load_balance.prefer_free_quota"

	// KeyPreferRicher — bool. Default false. When true, adds
	// subscription_balance DESC to the ORDER BY tail so the richest
	// account wins ties inside a tier.
	KeyPreferRicher = "load_balance.prefer_richer"

	// KeyBalanceHeadroomPct — int in [HeadroomMin, HeadroomMax].
	// Default 120. Percentage of estimated cost the account's
	// subscription_balance must exceed to qualify. 100 = no headroom;
	// 120 = the historical +20% buffer.
	KeyBalanceHeadroomPct = "load_balance.balance_headroom_pct"

	// KeyJitter — bool. Default true. When false, the RANDOM() ORDER
	// BY tail is omitted so PickAndLock becomes fully deterministic.
	KeyJitter = "load_balance.jitter"
)

// Defaults exposed as constants so tests and the handler share exact
// values. Keep in sync with the doc comments above.
const (
	DefaultTierAware          = true
	DefaultPreferUnlim        = false
	DefaultPreferFreeQuota    = false
	DefaultPreferRicher       = false
	DefaultBalanceHeadroomPct = 120
	DefaultJitter             = true

	HeadroomMin = 100
	HeadroomMax = 500
)

// Settings is the persisted / wire shape for GET / PUT
// /admin/settings/load_balance. Every field maps 1:1 to a
// system_settings row.
type Settings struct {
	TierAware          bool `json:"tier_aware"`
	PreferUnlim        bool `json:"prefer_unlim"`
	PreferFreeQuota    bool `json:"prefer_free_quota"`
	PreferRicher       bool `json:"prefer_richer"`
	BalanceHeadroomPct int  `json:"balance_headroom_pct"`
	Jitter             bool `json:"jitter"`
}

// Defaults returns the fallback settings applied when the
// system_settings row is absent for any key.
func Defaults() Settings {
	return Settings{
		TierAware:          DefaultTierAware,
		PreferUnlim:        DefaultPreferUnlim,
		PreferFreeQuota:    DefaultPreferFreeQuota,
		PreferRicher:       DefaultPreferRicher,
		BalanceHeadroomPct: DefaultBalanceHeadroomPct,
		Jitter:             DefaultJitter,
	}
}

// Resolve reads every load_balance.* key from the store and returns a
// filled Settings. Missing / unparseable values fall back to the
// default for that specific key (partial override supported). A nil
// store returns Defaults() unchanged.
//
// Cheap enough for the hot path: sqlite reads system_settings by
// primary key (b-tree lookup) and every key is a single scalar.
func Resolve(ctx context.Context, store ports.SettingsStore) Settings {
	out := Defaults()
	if store == nil {
		return out
	}
	out.TierAware = readBool(ctx, store, KeyTierAware, out.TierAware)
	out.PreferUnlim = readBool(ctx, store, KeyPreferUnlim, out.PreferUnlim)
	out.PreferFreeQuota = readBool(ctx, store, KeyPreferFreeQuota, out.PreferFreeQuota)
	out.PreferRicher = readBool(ctx, store, KeyPreferRicher, out.PreferRicher)
	out.Jitter = readBool(ctx, store, KeyJitter, out.Jitter)
	if v, err := store.Get(ctx, KeyBalanceHeadroomPct); err == nil {
		if n, perr := strconv.Atoi(v); perr == nil && n >= HeadroomMin && n <= HeadroomMax {
			out.BalanceHeadroomPct = n
		}
	}
	return out
}

// ToOpts converts a validated Settings into the ports.LoadBalanceOpts
// shape consumed by AccountStore.PickAndLock. Populated is set to true
// so the store applies these values instead of its hardcoded defaults.
func (s Settings) ToOpts() ports.LoadBalanceOpts {
	return ports.LoadBalanceOpts{
		Populated:          true,
		TierAware:          s.TierAware,
		PreferUnlim:        s.PreferUnlim,
		PreferFreeQuota:    s.PreferFreeQuota,
		PreferRicher:       s.PreferRicher,
		BalanceHeadroomPct: s.BalanceHeadroomPct,
		Jitter:             s.Jitter,
	}
}

// ParseBool accepts the canonical string encodings the handler writes
// ("true"/"false") plus "1"/"0" for tolerance against hand-edited
// rows. Anything else returns (false, false) so the caller treats the
// value as absent.
func ParseBool(v string) (bool, bool) {
	switch v {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

// FormatBool is the canonical string encoding written by the handler.
func FormatBool(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func readBool(ctx context.Context, store ports.SettingsStore, key string, fallback bool) bool {
	v, err := store.Get(ctx, key)
	if err != nil {
		return fallback
	}
	if b, ok := ParseBool(v); ok {
		return b
	}
	return fallback
}
