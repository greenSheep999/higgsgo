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
	path string

	mu      sync.RWMutex
	byAlias map[string]*domain.ModelSpec
	aliases map[string]aliasEntry
	locked  map[string]struct{}
}

type aliasEntry struct {
	BaseAlias string
	Strategy  domain.AliasStrategy
}

// Config controls Registry construction.
type Config struct {
	// Path to the verified-models.json produced by dump-verified-models.mjs.
	Path string

	// StarterLockedPath (optional) is a JSON list of JSTs that starter
	// accounts cannot open. When empty, starter-locked lookup returns
	// false for every JST.
	StarterLockedPath string
}

// New constructs a Registry and loads the file immediately.
func New(cfg Config) (*Registry, error) {
	r := &Registry{path: cfg.Path}
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

// Reload re-reads the JSON file and atomically swaps the in-memory maps.
func (r *Registry) Reload(_ context.Context) error {
	body, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("read model registry %q: %w", r.path, err)
	}
	var raw struct {
		Models []struct {
			Alias          string   `json:"alias"`
			JST            string   `json:"jst"`
			Endpoint       string   `json:"endpoint"`
			Version        string   `json:"version"`
			Output         string   `json:"output"`
			CostCreditsH   *int64   `json:"cost_credits_h"`
			RequiredParams []string `json:"required_params"`
			NeedsImage     bool     `json:"needs_image"`
			NeedsVideo     bool     `json:"needs_video"`
			NeedsAudio     bool     `json:"needs_audio"`
			NeedsMedias    bool     `json:"needs_medias"`
			AppSlug        string   `json:"app_slug"`
			SupportsUnlim  bool     `json:"supports_unlim"`
			UnlimJST       string   `json:"unlim_jst"`
			MediaRole      string   `json:"media_role"`
			Classification string   `json:"classification"`
		} `json:"models"`
		Aliases []struct {
			Alias     string `json:"alias"`
			BaseAlias string `json:"base_alias"`
			BaseJST   string `json:"base_jst"`
			Strategy  string `json:"strategy"`
		} `json:"aliases"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("parse model registry: %w", err)
	}

	byAlias := make(map[string]*domain.ModelSpec, len(raw.Models))
	for _, m := range raw.Models {
		spec := &domain.ModelSpec{
			Alias:           m.Alias,
			JST:             m.JST,
			Endpoint:        m.Endpoint,
			Version:         m.Version,
			Output:          m.Output,
			RequiredParams:  m.RequiredParams,
			MediaRole:       m.MediaRole,
			ApplicationSlug: m.AppSlug,
			Unstable:        strings.HasPrefix(m.Classification, "B"),
			Deprecated:      false,
		}
		if m.CostCreditsH != nil {
			spec.EstCostHundredths = *m.CostCreditsH
		}
		byAlias[m.Alias] = spec
	}

	aliasMap := make(map[string]aliasEntry, len(raw.Aliases))
	for _, a := range raw.Aliases {
		strategy := domain.AliasTransparent
		if a.Strategy == string(domain.AliasTryNativeFallback) {
			strategy = domain.AliasTryNativeFallback
		}
		aliasMap[a.Alias] = aliasEntry{
			BaseAlias: a.BaseAlias,
			Strategy:  strategy,
		}
	}

	r.mu.Lock()
	r.byAlias = byAlias
	r.aliases = aliasMap
	if r.locked == nil {
		r.locked = make(map[string]struct{})
	}
	r.mu.Unlock()
	return nil
}

// Resolve returns the ModelSpec for a user-facing alias, following alias
// entries transparently.
func (r *Registry) Resolve(alias string) (*domain.ModelSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if a, ok := r.aliases[alias]; ok && a.Strategy == domain.AliasTransparent {
		if spec, ok := r.byAlias[a.BaseAlias]; ok {
			// Return a shallow copy with the alias-of field set for observability.
			c := *spec
			c.AliasOf = a.BaseAlias
			c.AliasStrategy = domain.AliasTransparent
			return &c, nil
		}
	}
	spec, ok := r.byAlias[alias]
	if !ok {
		return nil, fmt.Errorf("%w: %q", domain.ErrModelNotFound, alias)
	}
	return spec, nil
}

// List returns all specs matching the filter. Order is not guaranteed.
func (r *Registry) List(filter ports.ModelFilter) []*domain.ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.ModelSpec, 0, len(r.byAlias))
	for _, spec := range r.byAlias {
		if filter.Output != "" && spec.Output != filter.Output {
			continue
		}
		if !filter.IncludeUnstable && spec.Unstable {
			continue
		}
		if !filter.IncludeDeprecated && spec.Deprecated {
			continue
		}
		out = append(out, spec)
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
