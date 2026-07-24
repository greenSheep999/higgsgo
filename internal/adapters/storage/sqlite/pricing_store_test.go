package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestPricingStore_SaveSnapshotIsAtomic(t *testing.T) {
	db := openMem(t)
	store := NewPricingStore(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	snapshot := &domain.PricingSnapshot{
		ID:            "price_atomic",
		Source:        "higgs_job_set_costs",
		SourceURL:     "/job-sets/costs",
		PayloadJSON:   `{"data":[]}`,
		PayloadSHA256: "sha",
		FetchedAt:     now,
	}
	rules := []domain.ModelCostRule{
		{ID: "rule_valid", JST: "model_a", Unit: "per_request", CreditsHundredths: 100, DimensionsJSON: `{}`, ObservedAt: now},
		{ID: "rule_invalid", JST: "model_b", Unit: "per_request", CreditsHundredths: 0, DimensionsJSON: `{}`, ObservedAt: now},
	}

	if err := store.SaveSnapshot(ctx, snapshot, rules); err == nil {
		t.Fatal("SaveSnapshot succeeded with an invalid rule")
	}

	for _, table := range []string{"pricing_snapshots", "model_cost_rules"} {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0 after rollback", table, count)
		}
	}
}

func TestPricingStore_ReturnsLatestSnapshotAndRules(t *testing.T) {
	db := openMem(t)
	store := NewPricingStore(db)
	ctx := context.Background()
	base := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)

	for i, tc := range []struct {
		id      string
		fetched time.Time
		jst     string
		credits int64
	}{
		{id: "price_old", fetched: base, jst: "old_model", credits: 100},
		{id: "price_new", fetched: base.Add(time.Hour), jst: "new_model", credits: 275},
	} {
		snapshot := &domain.PricingSnapshot{
			ID:            tc.id,
			Source:        "higgs_job_set_costs",
			SourceURL:     "/job-sets/costs",
			PayloadJSON:   `{"data":[]}`,
			PayloadSHA256: tc.id + "_sha",
			FetchedAt:     tc.fetched,
		}
		rules := []domain.ModelCostRule{{
			ID:                        "rule_" + tc.id,
			JST:                       tc.jst,
			Unit:                      "per_second",
			Component:                 "cost_per_second",
			CreditsHundredths:         tc.credits,
			OriginalCreditsHundredths: tc.credits + 100,
			Resolution:                "1080p",
			DimensionsJSON:            `{"resolution":"1080p"}`,
			ObservedAt:                tc.fetched,
		}}
		if err := store.SaveSnapshot(ctx, snapshot, rules); err != nil {
			t.Fatalf("SaveSnapshot[%d]: %v", i, err)
		}
	}

	latest, err := store.LatestSnapshot(ctx, "higgs_job_set_costs")
	if err != nil {
		t.Fatalf("LatestSnapshot: %v", err)
	}
	if latest == nil || latest.ID != "price_new" {
		t.Fatalf("latest snapshot = %+v, want price_new", latest)
	}
	rules, err := store.ListLatestRules(ctx, "higgs_job_set_costs")
	if err != nil {
		t.Fatalf("ListLatestRules: %v", err)
	}
	if len(rules) != 1 || rules[0].JST != "new_model" || rules[0].Component != "cost_per_second" ||
		rules[0].CreditsHundredths != 275 || rules[0].OriginalCreditsHundredths != 375 {
		t.Fatalf("latest rules = %+v, want only new_model at 275", rules)
	}

	missing, err := store.LatestSnapshot(ctx, "missing")
	if err != nil || missing != nil {
		t.Fatalf("missing snapshot = %+v, err=%v; want nil, nil", missing, err)
	}
}

func TestPricingStore_PricingMatrixSources(t *testing.T) {
	db := openMem(t)
	store := NewPricingStore(db)
	ctx := context.Background()

	rates, err := store.ListPlanCreditRates(ctx)
	if err != nil {
		t.Fatalf("ListPlanCreditRates: %v", err)
	}
	if len(rates) != 4 {
		t.Fatalf("plan rate count = %d, want 4; rates=%+v", len(rates), rates)
	}
	var starterFound bool
	for _, rate := range rates {
		if rate.PlanType == "starter" {
			starterFound = rate.UnitCostMicros == 75000 && rate.Credits == 200
		}
	}
	if !starterFound {
		t.Fatalf("starter plan baseline missing or wrong: %+v", rates)
	}

	official, err := store.ListOfficialPrices(ctx, "kling-3")
	if err != nil {
		t.Fatalf("ListOfficialPrices: %v", err)
	}
	// 12 rows = 8 INTL (from migration 029 verified Kling scrape) + 4 CN
	// (from migration 030's provider price backfill, CNY-derived estimates).
	// If either migration grows more variants, update this expectation.
	if len(official) != 12 {
		t.Fatalf("kling-3 official rows = %d, want 12", len(official))
	}
	// The canonical INTL 1080p × audio=on tuple ($0.168/s) MUST be
	// present — that's the reference row downstream front-end pins its
	// discount badge to.
	var intlAnchor bool
	for _, price := range official {
		if price.Region == "intl" && price.Resolution == "1080p" && price.Audio == "on" {
			intlAnchor = price.Unit == "per_second" && price.PriceMicros == 168000
		}
	}
	if !intlAnchor {
		t.Fatalf("INTL 1080p audio-on anchor missing: %+v", official)
	}

	decisions, err := store.ListPriceDecisions(ctx, "kling-3")
	if err != nil {
		t.Fatalf("ListPriceDecisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("seed must not invent final prices: %+v", decisions)
	}
}

// TestPricingStore_RecordPriceDecisionAppendsHistory confirms two writes to
// the same variant keep both rows, and ListPriceDecisions surfaces the most
// recent one first. That is the semantics the WebUI relies on: operators
// can revise a price without hiding the previous rationale.
func TestPricingStore_RecordPriceDecisionAppendsHistory(t *testing.T) {
	db := openMem(t)
	store := NewPricingStore(db)
	ctx := context.Background()

	// Same variant, second write two seconds after the first.
	older, err := store.RecordPriceDecision(ctx, domain.ModelPriceDecision{
		ModelAlias:  "kling-3",
		Unit:        "per_second",
		PriceMicros: 100000, // $0.10
		Resolution:  "720p",
		Mode:        "standard",
		Audio:       "off",
		Rationale:   "first pass",
		DecidedAt:   time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("first record: %v", err)
	}
	if older.ID == "" {
		t.Fatalf("stored decision should have an ID")
	}
	newer, err := store.RecordPriceDecision(ctx, domain.ModelPriceDecision{
		ModelAlias:  "kling-3",
		Unit:        "per_second",
		PriceMicros: 150000, // $0.15
		Resolution:  "720p",
		Mode:        "standard",
		Audio:       "off",
		Rationale:   "raised to hit 60% margin",
		DecidedAt:   time.Date(2026, 7, 23, 8, 0, 2, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	if newer.ID == older.ID {
		t.Fatalf("second write reused ID %q — expected a fresh row", newer.ID)
	}

	rows, err := store.ListPriceDecisions(ctx, "kling-3")
	if err != nil {
		t.Fatalf("ListPriceDecisions: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0].PriceMicros != 150000 {
		t.Fatalf("most recent row = %d, want 150000", rows[0].PriceMicros)
	}
	if rows[1].PriceMicros != 100000 {
		t.Fatalf("older row = %d, want 100000", rows[1].PriceMicros)
	}
}

// TestPricingStore_RecordPriceDecisionValidates covers the three input
// guards: missing alias, missing unit, and negative price. Each must fail
// without touching the table so the caller can surface a clean 400.
func TestPricingStore_RecordPriceDecisionValidates(t *testing.T) {
	db := openMem(t)
	store := NewPricingStore(db)
	ctx := context.Background()

	cases := []struct {
		name string
		in   domain.ModelPriceDecision
	}{
		{"no alias", domain.ModelPriceDecision{Unit: "per_second", PriceMicros: 100}},
		{"no unit", domain.ModelPriceDecision{ModelAlias: "kling-3", PriceMicros: 100}},
		{"negative price", domain.ModelPriceDecision{ModelAlias: "kling-3", Unit: "per_second", PriceMicros: -1}},
	}
	for _, tc := range cases {
		if _, err := store.RecordPriceDecision(ctx, tc.in); err == nil {
			t.Fatalf("%s: expected validation error, got nil", tc.name)
		}
	}
	// Verify no rows leaked in.
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM model_price_decisions").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("model_price_decisions leaked %d rows on validation failure", count)
	}
}
