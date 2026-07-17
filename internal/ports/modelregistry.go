package ports

import (
	"context"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// ModelRegistry resolves user-facing model aliases to internal ModelSpecs and
// enumerates the currently supported model set. Loaded from
// configs/models/*.toml and data/reference/sealed.json.
//
// The registry supports hot reload — a SIGHUP or POST /admin/models/reload
// re-parses the files and atomically swaps the in-memory map.
type ModelRegistry interface {
	// Resolve returns the ModelSpec for a user-facing alias, following
	// AliasOf chains transparently when AliasStrategy is AliasTransparent.
	Resolve(alias string) (*domain.ModelSpec, error)

	// List returns all specs matching the filter (empty filter = all).
	List(filter ModelFilter) []*domain.ModelSpec

	// Reload re-reads the config source and swaps the in-memory registry.
	Reload(ctx context.Context) error

	// ResolveAlias returns the target JST for an alias without descending
	// into ModelSpec details. Returns (target, true) if alias is registered.
	ResolveAlias(alias string) (string, bool)

	// StarterLocked reports whether the given JST is gated to paid tiers.
	StarterLocked(jst string) bool
}

// ModelFilter narrows a ModelRegistry.List call.
type ModelFilter struct {
	Output            string // "image" | "video" | "audio"; empty = any
	IncludeUnstable   bool
	IncludeDeprecated bool
}
