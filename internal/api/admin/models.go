package admin

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// reloadTimeoutVar caps a single admin-triggered Reload so a stuck file
// read or blocked registry swap cannot pin the admin request indefinitely.
// The jsonstatic backend just re-parses a JSON file, but future backends may
// hit remote sources; 30s is generous for both. Declared as a var (not a
// const) so tests can shrink it without waiting the full window.
var reloadTimeoutVar = 30 * time.Second

// ModelsHandler serves /admin/models/* endpoints for operators. Currently
// exposes a hot-reload trigger so operators can pick up edits to
// data/reference/verified-models.json without restarting the process.
type ModelsHandler struct {
	Registry ports.ModelRegistry
	Pricing  ports.PricingStore
	Logger   *slog.Logger

	// FloorReferenceUnitCostMicros and FloorMarkupMultiplier drive the
	// retail_below_floor warning attached to POST decisions. Zero values
	// disable the warning entirely (row still writes). See config.PricingConfig.
	FloorReferenceUnitCostMicros int64
	FloorMarkupMultiplier        float64
}

// NewModelsHandler builds a handler over the given registry. Registry must be
// non-nil; callers gate the mount at the server level so passing nil here is
// a programming error.
func NewModelsHandler(r ports.ModelRegistry, logger *slog.Logger) *ModelsHandler {
	return &ModelsHandler{Registry: r, Logger: logger}
}

// Register mounts the routes under /models plus the sibling
// /pricing/* catalog endpoints (floor-suggestions spans every alias
// and purchase-batches has no alias affinity at all, so they live
// under /pricing/ rather than /models/{alias}/*).
func (h *ModelsHandler) Register(r chi.Router) {
	r.Post("/models/reload", h.Reload)
	r.Get("/models/{alias}/pricing-matrix", h.PricingMatrix)
	r.Post("/models/{alias}/pricing-decisions", h.CreatePricingDecision)
	r.Get("/pricing/floor-suggestions", h.FloorSuggestions)
	r.Get("/pricing/purchase-batches", h.ListPurchaseBatches)
	r.Post("/pricing/purchase-batches", h.CreatePurchaseBatch)
	r.Put("/pricing/purchase-batches/{id}", h.UpdatePurchaseBatch)
	r.Delete("/pricing/purchase-batches/{id}", h.DeletePurchaseBatch)
}

// Reload re-reads the underlying registry source and swaps the in-memory
// maps. Returns previous_count / current_count so operators can eyeball the
// delta without a follow-up call.
func (h *ModelsHandler) Reload(w http.ResponseWriter, r *http.Request) {
	if h.Registry == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "model registry not configured")
		return
	}

	// Snapshot count before reload. IncludeUnstable + IncludeDeprecated so
	// the delta reflects every entry the registry knows about, not just the
	// production-visible subset.
	filter := ports.ModelFilter{IncludeUnstable: true, IncludeDeprecated: true}
	previous := len(h.Registry.List(filter))

	if h.Logger != nil {
		h.Logger.Info("admin trigger models reload", slog.Int("previous_count", previous))
	}

	ctx, cancel := context.WithTimeout(r.Context(), reloadTimeoutVar)
	defer cancel()

	if err := h.Registry.Reload(ctx); err != nil {
		if h.Logger != nil {
			h.Logger.Error("model registry reload failed", slog.String("error", err.Error()))
		}
		writeErr(w, http.StatusInternalServerError, "reload_failed", err.Error())
		return
	}

	current := len(h.Registry.List(filter))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"previous_count": previous,
		"current_count":  current,
		"reloaded_at":    time.Now().UTC().Format(time.RFC3339),
	})
}
