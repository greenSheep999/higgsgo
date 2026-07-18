// Package jsonstatic loads a ModelRegistry from a JSON file dumped by
// scripts/dump-verified-models.mjs. It is the primary registry for higgsgo
// — models are edited by re-running the dump script and calling Reload.
package jsonstatic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Registry implements ports.ModelRegistry.
type Registry struct {
	path      string
	extraPath string

	mu      sync.RWMutex
	byAlias map[string]*domain.ModelSpec
	aliases map[string]aliasEntry
	locked  map[string]struct{}

	// Optional override provider. When set, Reload() re-hydrates the
	// overrides map on top of the static catalog and every Resolve() /
	// List() call merges them in. Nil is a fully valid deployment (the
	// SQLite store simply isn't wired in tests or slimmer builds).
	overrideProvider ports.ModelOverrideStore
	// overrides is the merged snapshot keyed by canonical alias.
	// Rebuilt from overrideProvider.List() every Reload().
	overrides map[string]domain.ModelOverride
}

// extraSpec is the per-alias supplementary payload sourced from
// data/reference/model-specs-extra.json. Only fields the loader knows
// how to fold into ModelSpec are decoded; any other keys (e.g.
// _schema) are ignored.
type extraSpec struct {
	MaxResolution  string `json:"max_resolution"`
	MaxDurationSec int    `json:"max_duration_sec"`
}

type aliasEntry struct {
	BaseAlias string
	Strategy  domain.AliasStrategy
	// Tier fields override the base spec's tier when set. The Ultra-only
	// *_unlimited endpoints ride on the same base JST as their base alias
	// but require an Ultra-tier account, so we cannot inherit the base
	// spec's own tier verbatim.
	StarterLocked        bool
	RequiresPaid         bool
	RequiresUltra        bool
	RequiresUnlim        bool
	TierSource           string
	MinCreditsHundredths int64
}

// Config controls Registry construction.
type Config struct {
	// Path to the verified-models.json produced by dump-verified-models.mjs.
	Path string

	// StarterLockedPath (optional) is a JSON list of JSTs that starter
	// accounts cannot open. When empty, starter-locked lookup returns
	// false for every JST.
	StarterLockedPath string

	// ExtraSpecsPath (optional) points at a JSON file supplying
	// per-alias MaxResolution / MaxDurationSec. Missing / empty is
	// non-fatal — models without an entry render as "—" in the UI.
	// See data/reference/model-specs-extra.json for the shape.
	ExtraSpecsPath string
}

// New constructs a Registry and loads the file immediately.
func New(cfg Config) (*Registry, error) {
	r := &Registry{path: cfg.Path, extraPath: cfg.ExtraSpecsPath}
	if err := r.Reload(context.Background()); err != nil {
		return nil, err
	}
	if cfg.StarterLockedPath != "" {
		body, err := os.ReadFile(cfg.StarterLockedPath)
		if err != nil {
			return nil, fmt.Errorf("read starter-locked list: %w", err)
		}
		var list []string
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("parse starter-locked list: %w", err)
		}
		r.mu.Lock()
		for _, jst := range list {
			r.locked[jst] = struct{}{}
		}
		r.mu.Unlock()
	}
	return r, nil
}

// SetOverrideProvider wires (or unwires) a ModelOverrideStore. Once
// set, the very next Reload() (and every subsequent Resolve() / List()
// call) will see the operator overrides layered on top of the static
// catalog. main.go calls this immediately after constructing the
// registry so a boot with pending overrides in SQLite serves the
// merged view on the first request.
func (r *Registry) SetOverrideProvider(p ports.ModelOverrideStore) {
	r.mu.Lock()
	r.overrideProvider = p
	r.mu.Unlock()
}

// Reload re-reads the JSON file and atomically swaps the in-memory maps.
func (r *Registry) Reload(ctx context.Context) error {
	body, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("read model registry %q: %w", r.path, err)
	}
	var raw struct {
		Models []struct {
			Alias                string   `json:"alias"`
			JST                  string   `json:"jst"`
			Endpoint             string   `json:"endpoint"`
			Version              string   `json:"version"`
			Output               string   `json:"output"`
			CostCreditsH         *int64   `json:"cost_credits_h"`
			RequiredParams       []string `json:"required_params"`
			NeedsImage           bool     `json:"needs_image"`
			NeedsVideo           bool     `json:"needs_video"`
			NeedsAudio           bool     `json:"needs_audio"`
			NeedsMedias          bool     `json:"needs_medias"`
			AppSlug              string   `json:"app_slug"`
			SupportsUnlim        bool     `json:"supports_unlim"`
			UnlimJST             string   `json:"unlim_jst"`
			MediaRole            string   `json:"media_role"`
			Classification       string   `json:"classification"`
			StarterLocked        bool     `json:"starter_locked"`
			RequiresPaid         bool     `json:"requires_paid"`
			RequiresUltra        bool     `json:"requires_ultra"`
			RequiresUnlim        bool     `json:"requires_unlim"`
			TierSource           string   `json:"tier_source"`
			MinCreditsHundredths int64    `json:"min_credits_hundredths"`
		} `json:"models"`
		Aliases []struct {
			Alias                string `json:"alias"`
			BaseAlias            string `json:"base_alias"`
			BaseJST              string `json:"base_jst"`
			Strategy             string `json:"strategy"`
			StarterLocked        bool   `json:"starter_locked"`
			RequiresPaid         bool   `json:"requires_paid"`
			RequiresUltra        bool   `json:"requires_ultra"`
			RequiresUnlim        bool   `json:"requires_unlim"`
			TierSource           string `json:"tier_source"`
			MinCreditsHundredths int64  `json:"min_credits_hundredths"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("parse model registry: %w", err)
	}

	// Load the (optional) extras file up front so every spec built
	// below can be enriched in the same pass. A missing / empty file
	// is not an error — the loader just returns an empty map.
	extras, err := loadExtras(r.extraPath)
	if err != nil {
		return fmt.Errorf("load model extras: %w", err)
	}

	byAlias := make(map[string]*domain.ModelSpec, len(raw.Models))
	locked := make(map[string]struct{})
	for _, m := range raw.Models {
		spec := &domain.ModelSpec{
			Alias:                m.Alias,
			JST:                  m.JST,
			Endpoint:             m.Endpoint,
			Version:              m.Version,
			Output:               m.Output,
			RequiredParams:       m.RequiredParams,
			MediaRole:            m.MediaRole,
			ApplicationSlug:      m.AppSlug,
			Unstable:             strings.HasPrefix(m.Classification, "B"),
			Deprecated:           false,
			StarterLocked:        m.StarterLocked,
			RequiresPaid:         m.RequiresPaid,
			RequiresUltra:        m.RequiresUltra,
			RequiresUnlim:        m.RequiresUnlim,
			TierSource:           m.TierSource,
			MinCreditsHundredths: m.MinCreditsHundredths,
		}
		if m.CostCreditsH != nil {
			spec.EstCostHundredths = *m.CostCreditsH
		}
		// Derive user-facing tier + operational tags from the flags.
		spec.MinPlan = deriveMinPlan(spec)
		spec.Tags = deriveTags(spec)
		// Apply the max-output extras (if any) — safe no-op when the
		// alias has no entry.
		if e, ok := extras[m.Alias]; ok {
			spec.MaxResolution = e.MaxResolution
			spec.MaxDurationSec = e.MaxDurationSec
		}
		byAlias[m.Alias] = spec
		// Keep a JST-keyed locked set in sync with the per-spec flag so
		// StarterLocked(jst) callers don't need a linear scan.
		if m.StarterLocked && m.JST != "" {
			locked[m.JST] = struct{}{}
		}
	}

	aliasMap := make(map[string]aliasEntry, len(raw.Aliases))
	for _, a := range raw.Aliases {
		strategy := domain.AliasTransparent
		if a.Strategy == string(domain.AliasTryNativeFallback) {
			strategy = domain.AliasTryNativeFallback
		}
		aliasMap[a.Alias] = aliasEntry{
			BaseAlias:            a.BaseAlias,
			Strategy:             strategy,
			StarterLocked:        a.StarterLocked,
			RequiresPaid:         a.RequiresPaid,
			RequiresUltra:        a.RequiresUltra,
			RequiresUnlim:        a.RequiresUnlim,
			TierSource:           a.TierSource,
			MinCreditsHundredths: a.MinCreditsHundredths,
		}
	}

	r.mu.Lock()
	r.byAlias = byAlias
	r.aliases = aliasMap
	// Preserve any starter-locked JSTs supplied via the explicit
	// StarterLockedPath list (populated in New) while folding in the
	// per-spec starter_locked flag we just derived from the JSON file.
	if r.locked == nil {
		r.locked = make(map[string]struct{})
	}
	for jst := range locked {
		r.locked[jst] = struct{}{}
	}
	provider := r.overrideProvider
	r.mu.Unlock()

	// Hydrate overrides outside the write lock so a slow SQLite read
	// doesn't block concurrent Resolve() calls on the still-valid
	// stale catalog. The override provider is optional — a nil
	// provider (tests, slim deployments) leaves the overrides map
	// empty and behaviour matches the pre-015 registry.
	if provider != nil {
		list, err := provider.List(ctx)
		if err != nil {
			return fmt.Errorf("load model overrides: %w", err)
		}
		merged := make(map[string]domain.ModelOverride, len(list))
		for _, o := range list {
			merged[o.Alias] = o
		}
		r.mu.Lock()
		r.overrides = merged
		r.mu.Unlock()
	} else {
		r.mu.Lock()
		r.overrides = nil
		r.mu.Unlock()
	}
	return nil
}

// applyOverride returns a copy of spec with fields patched from o. A
// nil pointer in o means "no change"; a set pointer replaces the
// corresponding field on the copy. ExtraAliases / Note are always
// copied so an empty override still surfaces "no expansion" cleanly.
// Called with r.mu already RLock'd.
func applyOverride(spec domain.ModelSpec, o domain.ModelOverride) domain.ModelSpec {
	if o.StarterLocked != nil {
		spec.StarterLocked = *o.StarterLocked
	}
	if o.RequiresPaid != nil {
		spec.RequiresPaid = *o.RequiresPaid
	}
	if o.RequiresUltra != nil {
		spec.RequiresUltra = *o.RequiresUltra
	}
	if o.RequiresUnlim != nil {
		spec.RequiresUnlim = *o.RequiresUnlim
	}
	if o.MinCreditsHundredths != nil {
		spec.MinCreditsHundredths = *o.MinCreditsHundredths
	}
	if len(o.ExtraAliases) > 0 {
		copied := make([]string, len(o.ExtraAliases))
		copy(copied, o.ExtraAliases)
		spec.ExtraAliases = copied
	}
	if o.Note != "" {
		spec.Note = o.Note
	}
	return spec
}

// Resolve returns the ModelSpec for a user-facing alias, following alias
// entries transparently. Operator overrides layered by
// SetOverrideProvider() are merged in before the copy is returned.
func (r *Registry) Resolve(alias string) (*domain.ModelSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if a, ok := r.aliases[alias]; ok && a.Strategy == domain.AliasTransparent {
		if spec, ok := r.byAlias[a.BaseAlias]; ok {
			// Return a shallow copy with the alias-of field set for observability.
			c := *spec
			c.AliasOf = a.BaseAlias
			c.AliasStrategy = domain.AliasTransparent
			// Override tier fields on the alias only when it carries its
			// own tier attribution (TierSource != ""); otherwise let the
			// base spec's tier flow through unchanged.
			if a.TierSource != "" {
				c.StarterLocked = a.StarterLocked
				c.RequiresPaid = a.RequiresPaid
				c.RequiresUltra = a.RequiresUltra
				c.RequiresUnlim = a.RequiresUnlim
				c.TierSource = a.TierSource
				c.MinCreditsHundredths = a.MinCreditsHundredths
				// Tier flags shifted on the alias — re-derive the
				// user-facing MinPlan / Tags so the UI reflects the
				// alias-specific gate rather than the base spec's.
				c.MinPlan = deriveMinPlan(&c)
				c.Tags = deriveTags(&c)
			}
			// Merge the operator override (if any). The override key is
			// the *user-facing* alias, not the base spec's, so an
			// operator can pin `seedance-mini-unlimited` differently
			// from `seedance-2-0-mini`.
			if o, ok := r.overrides[alias]; ok {
				c = applyOverride(c, o)
				// Overrides can flip requires_* — refresh MinPlan / Tags.
				c.MinPlan = deriveMinPlan(&c)
				c.Tags = deriveTags(&c)
			}
			return &c, nil
		}
	}
	spec, ok := r.byAlias[alias]
	if !ok {
		return nil, fmt.Errorf("%w: %q", domain.ErrModelNotFound, alias)
	}
	if o, ok := r.overrides[alias]; ok {
		c := applyOverride(*spec, o)
		c.MinPlan = deriveMinPlan(&c)
		c.Tags = deriveTags(&c)
		return &c, nil
	}
	return spec, nil
}

// List returns all specs matching the filter, with operator overrides
// merged in. Order is not guaranteed. Every returned pointer is a
// fresh copy (safe for the caller to mutate).
func (r *Registry) List(filter ports.ModelFilter) []*domain.ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.ModelSpec, 0, len(r.byAlias))
	for alias, spec := range r.byAlias {
		if filter.Output != "" && spec.Output != filter.Output {
			continue
		}
		if !filter.IncludeUnstable && spec.Unstable {
			continue
		}
		if !filter.IncludeDeprecated && spec.Deprecated {
			continue
		}
		// Hand back a copy so a downstream mutation (or the merge
		// helper below) never leaks into the shared map. The copy is
		// cheap — ModelSpec is a small struct with two slice fields.
		c := *spec
		if o, ok := r.overrides[alias]; ok {
			c = applyOverride(c, o)
			c.MinPlan = deriveMinPlan(&c)
			c.Tags = deriveTags(&c)
		}
		out = append(out, &c)
	}
	return out
}

// ResolveAlias returns the target for an alias.
func (r *Registry) ResolveAlias(alias string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.aliases[alias]
	if !ok {
		return "", false
	}
	return a.BaseAlias, true
}

// StarterLocked reports whether the JST is on the starter-locked list.
func (r *Registry) StarterLocked(jst string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.locked[jst]
	return ok
}

// verify Registry implements the port at compile time.
var _ ports.ModelRegistry = (*Registry)(nil)

// asserting: ports.ModelRegistry embeds Reload, so this call is unused in
// production but keeps the import compiled during refactors.
var _ = errors.New

// deriveMinPlan collapses the four requires_* / starter_locked flags
// into a single user-facing PlanType: the minimum tier that satisfies
// the model's gate. Priority is set to match the accountCanRun rules
// in internal/api/admin/accounts.go — the first matching branch wins.
//
//	requires_ultra     → PlanUltra (the lowest tier accountCanRun
//	                     accepts for RequiresUltra models)
//	requires_unlim     → PlanPro   (has_unlim starts at Pro on the
//	                     current higgsfield plans)
//	requires_paid /    → PlanPro   (lowest PlanType.IsPaid tier)
//	  starter_locked
//	else               → PlanFree
//
// Deprecated / unstable are NOT plan gates; they're rendered as tags.
func deriveMinPlan(m *domain.ModelSpec) domain.PlanType {
	switch {
	case m.RequiresUltra:
		return domain.PlanUltra
	case m.RequiresUnlim:
		return domain.PlanPro
	case m.RequiresPaid || m.StarterLocked:
		return domain.PlanPro
	default:
		return domain.PlanFree
	}
}

// deriveTags collects operational classification labels that render
// separately from MinPlan in the admin UI. Order is stable: the caller
// can rely on a deterministic slice for tone / label mapping.
//
// Tag catalogue:
//
//	starter_locked   — the base flag; kept as a tag so operators can
//	                   still filter on "starter blocks this" even when
//	                   MinPlan == PlanPro subsumes it.
//	unlim_endpoint   — routes through /jobs/v2/*_unlimited (requires
//	                   the use_unlim:true parameter, which in turn
//	                   requires has_unlim on the account).
//	credit_gated     — soft credit floor: min_credits_hundredths > 0
//	                   or the loader tagged tier_source as
//	                   "credits_shortfall".
//	unstable         — B-classification models flagged during the
//	                   dump pass (upstream_fail signal).
//	deprecated       — retired endpoints kept for observability.
func deriveTags(m *domain.ModelSpec) []string {
	tags := make([]string, 0, 5)
	if m.StarterLocked {
		tags = append(tags, "starter_locked")
	}
	if m.RequiresUnlim {
		tags = append(tags, "unlim_endpoint")
	}
	if m.MinCreditsHundredths > 0 || m.TierSource == "credits_shortfall" {
		tags = append(tags, "credit_gated")
	}
	if m.Unstable {
		tags = append(tags, "unstable")
	}
	if m.Deprecated {
		tags = append(tags, "deprecated")
	}
	return tags
}

// loadExtras reads the optional model-specs-extra.json map. A path of
// "" or a non-existent file returns an empty map without error so
// callers (tests, slim deployments) can opt out cleanly. Any other
// I/O or parse error is returned to the caller — the extras file
// is authoritative and a silent corruption would surface as random
// blank cells in the admin table.
func loadExtras(path string) (map[string]extraSpec, error) {
	if path == "" {
		return map[string]extraSpec{}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]extraSpec{}, nil
		}
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	// The file wraps the actual map in an "aliases" key so we can
	// stash schema / provenance metadata alongside without polluting
	// the alias namespace. Missing "aliases" (or an empty file) is
	// treated as no extras.
	var wrapper struct {
		Aliases map[string]extraSpec `json:"aliases"`
	}
	if len(body) == 0 {
		return map[string]extraSpec{}, nil
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if wrapper.Aliases == nil {
		return map[string]extraSpec{}, nil
	}
	return wrapper.Aliases, nil
}
