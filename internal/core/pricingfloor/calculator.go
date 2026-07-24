// Package pricingfloor computes the effective per-credit unit cost
// from the purchase_batches table. This is the phase-2 replacement
// for the static `pricing.floor_reference_unit_cost_micros` config
// value (contract §10): the same number, but derived from actual
// procurement history so it tracks reality as prices drift.
//
// The single exported entry point is `EffectiveUnitCost` — a pure
// function taking a slice of batches and a fallback config value.
// It never touches storage directly; the caller passes rows in from
// ports.PricingStore.ListPurchaseBatches. This keeps the calculator
// trivially testable and reusable from any adapter (admin CRUD
// endpoint, floor-suggestions handler, POST decisions warning).
package pricingfloor

import (
	"github.com/greensheep999/higgsgo/internal/domain"
)

// Result is the calculator's return shape. Callers that only need the
// number can read `UnitCostMicros`; the extra fields exist so the
// admin UI can explain WHY the number came out the way it did.
type Result struct {
	// UnitCostMicros is the weighted-average USD × 1e6 per credit.
	// Zero means "no eligible batches found; use config fallback".
	UnitCostMicros int64
	// TotalPaidMicros is the numerator: sum of every eligible
	// batch's total_paid_micros.
	TotalPaidMicros int64
	// TotalCredits is the denominator: sum of
	// credits_per_account × accounts_count across eligible batches
	// (in whole credits, not hundredths).
	TotalCredits int64
	// EligibleBatches is the count of batches that made it past the
	// filter, for "N batches / X credits" summary lines in the UI.
	EligibleBatches int
	// FallbackApplied is true when eligible batches produced no
	// denominator (empty table, all inactive, all unlim_1day, etc.)
	// and the caller-supplied fallback is what's in UnitCostMicros.
	FallbackApplied bool
}

// EffectiveUnitCost computes the credit-weighted average unit cost
// across every batch that is:
//
//   - Active (Active=true)
//   - Normal-class (PricingClass=="normal" — excludes activity, bug,
//     promo deals flagged as one-off outliers by the operator)
//   - Non-promotional (PromotionType=="none" or "" — excludes
//     first_signup, unlim_1day, standard_credit_boost deals whose
//     price does not reflect the baseline market rate for a plain
//     credit purchase)
//   - Credit-bearing (CreditsPerAccountHundredths > 0 — filters out
//     any structurally-zero denominator).
//   - Positively priced (TotalPaidMicros > 0 — a defense against
//     data-entry errors that would divide-by-zero the numerator
//     when a batch is later flipped inactive).
//
// If no batch survives the filter, `fallbackUnitCost` is returned
// with `FallbackApplied=true`. This keeps the pricing floor working
// during the transitional period when the table is empty or the
// operator is still cleaning up seed rows.
//
// The math:
//
//	unit_cost = SUM(total_paid_micros)
//	          / SUM(credits_per_account_hundredths / 100 × accounts_count)
//
// Credits are stored in hundredths for integer safety, so we scale
// them back to whole credits before dividing. Integer division at
// the very end truncates fractional micros — a rounding error of at
// most 1 µ/cr, negligible against the 25_000+ range this number
// lives in.
func EffectiveUnitCost(batches []domain.PurchaseBatch, fallbackUnitCost int64) Result {
	var totalPaid, totalCreditsHundredths int64
	eligible := 0
	for _, b := range batches {
		if !b.Active {
			continue
		}
		if b.PricingClass != "normal" {
			continue
		}
		// Empty PromotionType is treated as "none" for backward
		// compatibility with pre-migration-026 rows the calculator
		// may see through some non-store data path (test fixtures,
		// mocks). Storage layer normalizes to "none" on write.
		if b.PromotionType != "" && b.PromotionType != "none" {
			continue
		}
		if b.CreditsPerAccountHundredths <= 0 {
			continue
		}
		if b.TotalPaidMicros <= 0 {
			continue
		}
		if b.AccountsCount <= 0 {
			continue
		}
		totalPaid += b.TotalPaidMicros
		totalCreditsHundredths += b.CreditsPerAccountHundredths * int64(b.AccountsCount)
		eligible++
	}
	if totalCreditsHundredths == 0 {
		return Result{
			UnitCostMicros:  fallbackUnitCost,
			FallbackApplied: true,
		}
	}
	// Convert hundredths to whole credits at the last step to
	// preserve precision.
	totalCredits := totalCreditsHundredths / 100
	if totalCredits == 0 {
		return Result{
			UnitCostMicros:  fallbackUnitCost,
			FallbackApplied: true,
		}
	}
	return Result{
		UnitCostMicros:  totalPaid / totalCredits,
		TotalPaidMicros: totalPaid,
		TotalCredits:    totalCredits,
		EligibleBatches: eligible,
	}
}
