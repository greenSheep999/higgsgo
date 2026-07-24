package admin

import (
	"context"
	"log/slog"

	"github.com/greensheep999/higgsgo/internal/core/pricingfloor"
)

// effectiveFloorUnitCost resolves the per-credit reference unit cost
// the pricing floor should use RIGHT NOW. Phase-2 (contract §10):
//
//  1. Ask the purchase_batches table for a credit-weighted average
//     across every active + normal-class batch on record.
//  2. If any eligible batch exists, that's the number.
//  3. Otherwise, fall back to the config constant
//     (FloorReferenceUnitCostMicros) so the floor keeps working
//     during rollout / on fresh test DBs.
//
// The returned `dynamic` flag lets callers surface which layer won
// in log messages and warning payloads — "cost is 20_670µ from
// batches" reads very differently than "cost is 27_500µ from
// config default", and operators care about the distinction when
// pricing decisions get flagged.
//
// Store errors are logged (best-effort) and treated as "no data
// available" → fallback. We do NOT propagate the error up because
// the calling paths (POST decisions warning, floor-suggestions
// response) already have a well-defined behavior for "no batches"
// and adding a hard-fail branch would make the pricing surfaces
// brittle to unrelated storage hiccups.
func (h *ModelsHandler) effectiveFloorUnitCost(ctx context.Context) (unitCost int64, dynamic bool) {
	fallback := h.FloorReferenceUnitCostMicros
	if h.Pricing == nil {
		return fallback, false
	}
	batches, err := h.Pricing.ListPurchaseBatches(ctx)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Warn("floor unit cost: purchase batches read failed, falling back to config",
				slog.String("err", err.Error()),
				slog.Int64("fallback_micros", fallback))
		}
		return fallback, false
	}
	result := pricingfloor.EffectiveUnitCost(batches, fallback)
	return result.UnitCostMicros, !result.FallbackApplied
}
