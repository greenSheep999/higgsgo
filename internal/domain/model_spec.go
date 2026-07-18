package domain

// ModelSpec is the resolved specification for a user-requested model alias.
// Loaded from configs/models/*.toml by the ModelRegistry (see internal/ports).
type ModelSpec struct {
	// Alias is what the API user types (e.g., "seedance-2", "veo3-1-fast").
	Alias string

	// JST (job_set_type) is higgsfield's internal snake_case identifier
	// (e.g., "seedance_2_0", "veo3_1").
	JST string

	// Endpoint is the full path relative to https://fnf.higgsfield.ai
	// (e.g., "/jobs/v2/seedance_2_0" or "/jobs/veo3-1").
	Endpoint string

	// Version is the endpoint family: "v1" (hyphenated) or "v2" (snake_case /v2 path).
	Version string

	// Output type: "image" | "video" | "audio".
	Output string

	// Gate flags.
	StarterLocked bool // Starter tier cannot use this model
	RequiresPaid  bool // any paid tier suffices
	RequiresUltra bool // /jobs/v2/*_unlimited endpoints
	RequiresUnlim bool // use_unlim:true parameter is required for correct routing

	// Cost estimation (credits * 100 per generation at default resolution/duration).
	// Used for budget checks in the account pool.
	EstCostHundredths int64

	// TierSource is a short label recording where the tier flags above
	// were derived from (e.g. "sealed:veo3_family_pro", "credits_shortfall",
	// "mapping:image_models.mjs", "assumed_starter_safe"). Populated by the
	// dump script; safe to inspect from admin UIs but not used for gating.
	TierSource string

	// MinCreditsHundredths is a soft credit floor: when non-zero, the pool
	// selector must skip accounts whose subscription_balance is below this
	// value. Layered on top of the hard-tier flags for credit-intensive
	// models so we don't burn a paid account with insufficient balance.
	MinCreditsHundredths int64

	// RequiredParams from empirical 422 responses.
	RequiredParams []string

	// Enums maps param name to allowed values (empirically discovered).
	Enums map[string][]string

	// MediaRole is the role name used inside params.medias[] for user-supplied
	// images (e.g., "start_image" for seedance, "image" for nano_banana).
	MediaRole string

	// ExampleBody is a working request body captured from a completed job.
	// Serialized JSON.
	ExampleBodyJSON string

	// ApplicationSlug for models in the nano_banana_2 family. Required at the
	// top level of the request body (NOT inside params). See catalog.mjs.
	ApplicationSlug string

	// AliasOf indicates this model is a proxy-through alias of another.
	// Used to map *_unlimited variants to their base counterparts.
	AliasOf       string
	AliasStrategy AliasStrategy

	// Stability flags.
	Unstable   bool   // known to have B_upstream_fail issues
	Deprecated bool   // old endpoint the SPA no longer promotes
	VerifiedAt string // ISO date of last successful end-to-end verification
	VerifiedBy []string

	// ExtraAliases are additional public names the model should be
	// advertised under on /v1/models. Only affects the outbound catalog
	// view so downstream aggregators (new-api) can register the same
	// model under several names. higgsgo's own routing / auth / metering
	// still key off Alias. Populated by ModelOverrideStore.
	ExtraAliases []string

	// Note is a free-form operator memo attached to the model via
	// ModelOverrideStore. Surfaced in the admin surface so operators
	// can leave a "why this override exists" hint.
	Note string

	// MinPlan is the human-readable minimum plan tier required to run
	// this model, sourced from official higgsfield plan names in
	// PlanType. Empty means "free tier / no plan requirement".
	// Derived by the loader from the four requires_* / starter_locked
	// flags according to accountCanRun's rules — see the mapping in
	// internal/adapters/modelregistry/jsonstatic/registry.go. UI-only
	// hint: the pool router still gates via the individual flags.
	MinPlan PlanType

	// Tags are internal / operational classification labels rendered
	// separately from MinPlan. Values are stable slugs the UI maps to
	// tone + label (e.g. "starter_locked", "unlim_endpoint",
	// "credit_gated", "unstable", "deprecated"). Order is stable so
	// downstream renders are deterministic.
	Tags []string

	// MaxResolution is the highest resolution this model can emit,
	// expressed in the same short form used by higgsfield's own
	// catalog ("480p" | "720p" | "1080p" | "4k" | "1024x1024" |
	// "2k"...). Empty when data/reference/model-specs-extra.json
	// didn't supply a value.
	MaxResolution string

	// MaxDurationSec is the longest duration this model can produce,
	// in seconds. Populated for video / audio models where the catalog
	// exposes a fixed upper bound; 0 when unknown or when duration is
	// not applicable (image models).
	MaxDurationSec int
}

// AliasStrategy defines how an alias is resolved.
type AliasStrategy string

const (
	// AliasTransparent forwards the request to the target model silently.
	// The user does not see that a fallback happened.
	AliasTransparent AliasStrategy = "transparent"

	// AliasTryNativeFallback attempts the native endpoint first (e.g.,
	// /jobs/v2/seedance_2_unlimited) and falls back to the base if 403.
	// Requires at least one Ultra-tier account in the pool.
	AliasTryNativeFallback AliasStrategy = "try_native_fallback"
)
