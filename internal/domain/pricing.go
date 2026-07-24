package domain

import "time"

// PricingSnapshot is one immutable upstream pricing payload.
type PricingSnapshot struct {
	ID            string
	Source        string
	SourceURL     string
	PayloadJSON   string
	PayloadSHA256 string
	FetchedAt     time.Time
	EffectiveAt   time.Time
}

// ModelCostRule is one normalized priced parameter combination from a
// PricingSnapshot. CreditsHundredths is always credits * 100.
type ModelCostRule struct {
	ID                string
	SnapshotID        string
	JST               string
	ModelAlias        string
	Unit              string
	Component         string
	CreditsHundredths int64
	// OriginalCreditsHundredths retains an upstream pre-discount/reference
	// value when the same rule includes one. Zero means not supplied.
	OriginalCreditsHundredths int64
	Resolution                string
	DurationSeconds           int
	Mode                      string
	Audio                     string
	DimensionsJSON            string
	ObservedAt                time.Time
}

// PlanCreditRate is the money cost of one Higgs credit for an official plan.
// Monetary values use USD micros to avoid floating-point drift.
type PlanCreditRate struct {
	ID             string
	PlanType       string
	PlanName       string
	BillingPeriod  string
	Currency       string
	AmountMicros   int64
	Credits        int64
	UnitCostMicros int64
	SourceURL      string
	ObservedAt     time.Time
}

// OfficialPriceObservation is one provider-published API price at a concrete
// parameter combination.
type OfficialPriceObservation struct {
	ID              string
	ModelAlias      string
	Provider        string
	SourceURL       string
	Currency        string
	Unit            string
	PriceMicros     int64
	Resolution      string
	DurationSeconds int
	Mode            string
	Audio           string
	DimensionsJSON  string
	ObservedAt      time.Time
	// Region: "intl" (default) or "cn". Added in migration 028 so the
	// matrix UI can show 海外 API and 国内 API side by side.
	Region string
	// Estimated: true when the price is derived (currency conversion,
	// third-party proxy) rather than lifted verbatim from the provider's
	// official pricing page. UI renders an "(估算)" badge; floor logic
	// should ignore these rows.
	Estimated bool
}

// PurchaseBatch records one real-world procurement event — buying a
// Higgs account or a top-up — so the effective unit cost feeding the
// pricing floor (contract §10) can be derived from actual purchase
// history rather than a hand-picked config constant.
//
// A batch is IMMUTABLE historical fact: the linked account may be
// disabled or even purged from `accounts` later, but the batch stays.
// `Active=false` retires a row from the weighted average without
// deleting it (audit trail preserved). PricingClass="normal" rows are
// the default input to the average; "activity" / "bug" / "promo" are
// excluded so a one-off deal doesn't drag the floor down.
type PurchaseBatch struct {
	ID                          string
	PurchasedAt                 time.Time
	SourceChannel               string // e.g. "tg", "taobao", "xianyu", "wechat", "official"
	SourceSeller                string // e.g. "BLACKHATWORLD", "CheapLuxuryAI" — scoped inside SourceChannel
	PlanType                    string // "starter" / "pro" / "plus" / "ultra" / "unlim_1day"
	AccountsCount               int
	CreditsPerAccountHundredths int64 // 0 for unlim_1day (no credit denominator)
	TotalPaidMicros             int64 // Always USD micros (converted at ExchangeRateUsed if paid in another currency)
	PaidCurrency                string
	PaidAmountOriginalMicros    int64
	ExchangeRateUsed            float64
	PricingClass                string // "normal" | "activity" | "bug" | "promo" — outlier tag
	// PromotionType describes which promotional path this purchase took.
	// Values:
	//   none                   — regular purchase, no promo layer
	//   first_signup           — free registration bonus (NOT normally
	//                            in this table; documented for
	//                            completeness)
	//   unlim_1day             — base plan + 1 day of unlimited quota
	//   standard_credit_boost  — reduced per-credit rate deal
	// The calculator filters PromotionType="none" (alongside
	// PricingClass="normal") when computing the weighted average, so
	// promo batches record real spend without skewing the baseline.
	PromotionType      string
	Active             bool
	LinkedAccountEmail string // Nullable-in-practice; empty when the account was purged after purchase
	Rationale                   string
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

// ModelPriceDecision is higgsgo's final operator-approved sell price for one
// parameter combination, independent from every upstream cost source.
type ModelPriceDecision struct {
	ID              string
	ModelAlias      string
	Currency        string
	Unit            string
	PriceMicros     int64
	Resolution      string
	DurationSeconds int
	Mode            string
	Audio           string
	DimensionsJSON  string
	Rationale       string
	DecidedAt       time.Time
}
