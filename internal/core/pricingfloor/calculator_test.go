package pricingfloor

import (
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// batch is a helper to keep test fixtures short. All required fields
// have sensible defaults; a test only overrides what it's exercising.
func batch(overrides func(*domain.PurchaseBatch)) domain.PurchaseBatch {
	b := domain.PurchaseBatch{
		Active:                      true,
		PricingClass:                "normal",
		AccountsCount:               1,
		CreditsPerAccountHundredths: 20000, // 200 credits
		TotalPaidMicros:             5_610_000,
	}
	if overrides != nil {
		overrides(&b)
	}
	return b
}

// TestEffectiveUnitCost_SeedFixture reproduces the actual weighted
// average across the 10 seed batches from migration 025. Excludes
// unlim_1day (credits=0) and the plus row's mistaken pricing is
// already fixed to $11.20 in the fixture. Expected numerator /
// denominator sanity-checked by hand:
//
//	Numerator: 6 × 5_610_000 (starter @ $5.61 × 6 sellers)
//	         + 1 × 5_340_000 (starter @ $5.34, later CheapLuxury row)
//	         + 1 × 11_200_000 (plus @ $11.20, corrected from typo)
//	         + 1 × 11_810_000 (pro @ $11.81)
//	         = 33_660_000 + 5_340_000 + 11_200_000 + 11_810_000
//	         = 62_010_000 micros ($62.01)
//	Denominator: 7 × 200 + 1_000 + 600 = 3_000 credits
//	  (7 starter accounts × 200cr + 1 plus × 1000cr + 1 pro × 600cr)
//	Unit cost = 62_010_000 / 3_000 = 20_670 micros/cr
//
// This is the exact number the pricing floor should use on day 1.
func TestEffectiveUnitCost_SeedFixture(t *testing.T) {
	batches := []domain.PurchaseBatch{
		batch(func(b *domain.PurchaseBatch) { b.ID = "b1" }),
		batch(func(b *domain.PurchaseBatch) { b.ID = "b2" }),
		batch(func(b *domain.PurchaseBatch) { b.ID = "b3" }),
		batch(func(b *domain.PurchaseBatch) { b.ID = "b4" }),
		batch(func(b *domain.PurchaseBatch) { b.ID = "b5" }),
		batch(func(b *domain.PurchaseBatch) { b.ID = "b6" }),
		batch(func(b *domain.PurchaseBatch) {
			b.ID = "b7_unlim"
			// Post-migration-026 shape: base plan is still starter
			// (200 credits), but promotion_type flags the 1day-unlim
			// promo layer. Excluded from the weighted average by the
			// promotion filter, NOT by credits=0 anymore.
			b.PlanType = "starter"
			b.PromotionType = "unlim_1day"
			b.CreditsPerAccountHundredths = 20_000
			b.TotalPaidMicros = 2_860_000
		}),
		batch(func(b *domain.PurchaseBatch) {
			b.ID = "b8_plus"
			b.PlanType = "plus"
			b.CreditsPerAccountHundredths = 100_000 // 1000 credits
			b.TotalPaidMicros = 11_200_000
		}),
		batch(func(b *domain.PurchaseBatch) {
			b.ID = "b9"
			b.TotalPaidMicros = 5_340_000
		}),
		batch(func(b *domain.PurchaseBatch) {
			b.ID = "b10_pro"
			b.PlanType = "pro"
			b.CreditsPerAccountHundredths = 60_000 // 600 credits
			b.TotalPaidMicros = 11_810_000
		}),
	}
	got := EffectiveUnitCost(batches, 27_500)
	if got.FallbackApplied {
		t.Fatalf("fallback should not apply with 9 eligible batches")
	}
	if got.EligibleBatches != 9 {
		t.Fatalf("eligible = %d, want 9 (excludes unlim_1day)", got.EligibleBatches)
	}
	if got.TotalPaidMicros != 62_010_000 {
		t.Fatalf("total paid = %d, want 62_010_000", got.TotalPaidMicros)
	}
	if got.TotalCredits != 3_000 {
		t.Fatalf("total credits = %d, want 3_000", got.TotalCredits)
	}
	// 62_010_000 / 3_000 = 20_670
	if got.UnitCostMicros != 20_670 {
		t.Fatalf("unit cost = %d, want 20_670", got.UnitCostMicros)
	}
}

// TestEffectiveUnitCost_EmptyFallback covers the transitional case
// where the table is empty (no phase-2 rollout yet, or a fresh test
// DB). The fallback config value MUST come through unchanged so the
// pricing floor keeps working.
func TestEffectiveUnitCost_EmptyFallback(t *testing.T) {
	got := EffectiveUnitCost(nil, 27_500)
	if !got.FallbackApplied {
		t.Fatalf("fallback should apply when input is empty")
	}
	if got.UnitCostMicros != 27_500 {
		t.Fatalf("unit cost = %d, want fallback 27_500", got.UnitCostMicros)
	}
	if got.EligibleBatches != 0 {
		t.Fatalf("eligible = %d, want 0", got.EligibleBatches)
	}
}

// TestEffectiveUnitCost_ExcludesInactive proves an operator can
// retire an outlier batch by flipping Active=false without deleting
// it. The retired row must NOT contribute to numerator or
// denominator.
func TestEffectiveUnitCost_ExcludesInactive(t *testing.T) {
	got := EffectiveUnitCost([]domain.PurchaseBatch{
		batch(nil),                                                  // eligible
		batch(func(b *domain.PurchaseBatch) { b.Active = false }),   // retired
	}, 27_500)
	if got.EligibleBatches != 1 {
		t.Fatalf("eligible = %d, want 1 (retired batch excluded)", got.EligibleBatches)
	}
	// 5_610_000 / 200 = 28_050
	if got.UnitCostMicros != 28_050 {
		t.Fatalf("unit cost = %d, want 28_050", got.UnitCostMicros)
	}
}

// TestEffectiveUnitCost_ExcludesNonNormalClass proves activity /
// bug / promo classes are filtered out. Feeds one normal batch at
// $5.61 alongside an artificially-cheap "activity" batch at $1.00;
// the activity row MUST NOT drag the average down.
func TestEffectiveUnitCost_ExcludesNonNormalClass(t *testing.T) {
	got := EffectiveUnitCost([]domain.PurchaseBatch{
		batch(nil),                                                             // normal
		batch(func(b *domain.PurchaseBatch) { b.PricingClass = "activity"; b.TotalPaidMicros = 1_000_000 }),
		batch(func(b *domain.PurchaseBatch) { b.PricingClass = "bug"; b.TotalPaidMicros = 100_000 }),
		batch(func(b *domain.PurchaseBatch) { b.PricingClass = "promo"; b.TotalPaidMicros = 500_000 }),
	}, 27_500)
	if got.EligibleBatches != 1 {
		t.Fatalf("eligible = %d, want 1 (non-normal excluded)", got.EligibleBatches)
	}
	if got.UnitCostMicros != 28_050 {
		t.Fatalf("unit cost = %d, want 28_050 (only the normal row counts)", got.UnitCostMicros)
	}
}

// TestEffectiveUnitCost_MultipleAccountsPerBatch verifies that a
// batch representing "bought 10 accounts at once" is weighted by
// AccountsCount, not just credited once. This is a common real-world
// case (bulk deal on Xianyu, etc.) and the seed schema supports it.
func TestEffectiveUnitCost_MultipleAccountsPerBatch(t *testing.T) {
	got := EffectiveUnitCost([]domain.PurchaseBatch{
		batch(func(b *domain.PurchaseBatch) {
			b.AccountsCount = 10
			b.CreditsPerAccountHundredths = 20_000 // 200 credits each
			b.TotalPaidMicros = 50_000_000         // $50 for the 10-pack
		}),
	}, 27_500)
	// Expected: $50 / (10 × 200) = 25_000 micros/cr
	if got.EligibleBatches != 1 {
		t.Fatalf("eligible = %d, want 1", got.EligibleBatches)
	}
	if got.TotalCredits != 2_000 {
		t.Fatalf("total credits = %d, want 2_000 (10 accts × 200 cr)", got.TotalCredits)
	}
	if got.UnitCostMicros != 25_000 {
		t.Fatalf("unit cost = %d, want 25_000", got.UnitCostMicros)
	}
}

// TestEffectiveUnitCost_ExcludesPromoTypes proves the calculator
// filters promotion_type != "none". Feeds one baseline batch at
// standard pricing and three promo batches at artificially-low
// prices; the promos MUST NOT drag the average down.
//
// This is the phase-2 refinement of the "activity/bug/promo"
// filter: promotion_type is orthogonal to pricing_class. A row
// can be pricing_class=normal + promotion_type=unlim_1day, meaning
// "typical unlim promo purchase" — still promotional, still
// excluded.
func TestEffectiveUnitCost_ExcludesPromoTypes(t *testing.T) {
	got := EffectiveUnitCost([]domain.PurchaseBatch{
		batch(nil), // baseline
		batch(func(b *domain.PurchaseBatch) {
			b.PromotionType = "first_signup"
			b.TotalPaidMicros = 0
		}),
		batch(func(b *domain.PurchaseBatch) {
			b.PromotionType = "unlim_1day"
			b.TotalPaidMicros = 2_860_000
		}),
		batch(func(b *domain.PurchaseBatch) {
			b.PromotionType = "standard_credit_boost"
			b.TotalPaidMicros = 3_000_000
		}),
	}, 27_500)
	if got.EligibleBatches != 1 {
		t.Fatalf("eligible = %d, want 1 (only the baseline row)", got.EligibleBatches)
	}
	// baseline: 5_610_000 / 200 = 28_050
	if got.UnitCostMicros != 28_050 {
		t.Fatalf("unit cost = %d, want 28_050", got.UnitCostMicros)
	}
}

// TestEffectiveUnitCost_EmptyPromoTreatedAsNone documents the
// backward-compat behavior: a batch with PromotionType="" (empty)
// counts as "none" so pre-migration-026 test fixtures and any row
// that skipped the storage normalize step still contribute.
func TestEffectiveUnitCost_EmptyPromoTreatedAsNone(t *testing.T) {
	got := EffectiveUnitCost([]domain.PurchaseBatch{
		batch(func(b *domain.PurchaseBatch) { b.PromotionType = "" }),
	}, 27_500)
	if got.EligibleBatches != 1 {
		t.Fatalf("empty PromotionType should be treated as none (eligible), got %d", got.EligibleBatches)
	}
	if got.UnitCostMicros != 28_050 {
		t.Fatalf("unit cost = %d, want 28_050", got.UnitCostMicros)
	}
}
