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
