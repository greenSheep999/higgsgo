package sqlite

// Tests for UsageEventStore: Insert, Query filters/pagination, Aggregate
// grouping and metrics, and the GroupBy whitelist. All backed by an
// in-memory SQLite handle via openMem (see sqlite_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// newUsageEvent returns a UsageEvent populated with sensible defaults so
// individual tests only need to override the fields they care about.
func newUsageEvent(id string) *domain.UsageEvent {
	ts := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	return &domain.UsageEvent{
		ID:                       id,
		TS:                       ts,
		APIKeyID:                 "key_a",
		GroupID:                  "grp_default",
		AccountID:                "acc_1",
		ModelAlias:               "video-flash",
		JST:                      "text2video_seedance",
		MediaType:                "video",
		UpstreamCost:             1000,
		ActualCreditsHundredths:  1000,
		ChargedCreditsHundredths: 1500,
		MarkupPct:                1.5,
		Status:                   domain.JobCompleted,
		LatencyMS:                4200,
		PollCount:                3,
		HiggsgoJobID:             "job_" + id,
		BillingMonth:             ts.Format("2006-01"),
		BillingDay:               ts.Format("2006-01-02"),
	}
}

func TestUsageEventStore_InsertAndQuery(t *testing.T) {
	db := openMem(t)
	store := NewUsageEventStore(db)
	ctx := context.Background()

	// Three rows spread across api_key_id / model_alias / billing_day so
	// each filter path can be exercised in isolation.
	day1 := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	day3 := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)

	e1 := newUsageEvent("u1")
	e1.TS = day1
	e1.BillingDay = day1.Format("2006-01-02")
	e1.APIKeyID = "key_a"
	e1.ModelAlias = "video-flash"

	e2 := newUsageEvent("u2")
	e2.TS = day2
	e2.BillingDay = day2.Format("2006-01-02")
	e2.APIKeyID = "key_b"
	e2.ModelAlias = "image-nano"
	e2.MediaType = "image"

	e3 := newUsageEvent("u3")
	e3.TS = day3
	e3.BillingDay = day3.Format("2006-01-02")
	e3.APIKeyID = "key_a"
	e3.ModelAlias = "image-nano"
	e3.MediaType = "image"

	for _, ev := range []*domain.UsageEvent{e1, e2, e3} {
		if err := store.Insert(ctx, ev); err != nil {
			t.Fatalf("insert %s: %v", ev.ID, err)
		}
	}

	// Filter by APIKeyID = key_a should yield e3, e1 (newest first).
	got, err := store.Query(ctx, ports.UsageQuery{APIKeyID: "key_a"})
	if err != nil {
		t.Fatalf("query by api key: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("api key filter: got %d rows want 2 (%v)", len(got), idList(got))
	}
	if got[0].ID != "u3" || got[1].ID != "u1" {
		t.Errorf("api key filter order: got %v want [u3 u1]", idList(got))
	}

	// Filter by ModelAlias narrows further.
	got, err = store.Query(ctx, ports.UsageQuery{ModelAlias: "image-nano"})
	if err != nil {
		t.Fatalf("query by model: %v", err)
	}
	if len(got) != 2 || got[0].ID != "u3" || got[1].ID != "u2" {
		t.Errorf("model filter: got %v want [u3 u2]", idList(got))
	}

	// Filter by Since/Until: [day2, day3) should return only u2.
	got, err = store.Query(ctx, ports.UsageQuery{Since: day2, Until: day3})
	if err != nil {
		t.Fatalf("query by window: %v", err)
	}
	if len(got) != 1 || got[0].ID != "u2" {
		t.Errorf("window filter: got %v want [u2]", idList(got))
	}

	// Limit / Offset pagination on the full set.
	page1, err := store.Query(ctx, ports.UsageQuery{Limit: 2})
	if err != nil {
		t.Fatalf("query page1: %v", err)
	}
	if len(page1) != 2 || page1[0].ID != "u3" || page1[1].ID != "u2" {
		t.Errorf("page1: got %v want [u3 u2]", idList(page1))
	}
	page2, err := store.Query(ctx, ports.UsageQuery{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("query page2: %v", err)
	}
	if len(page2) != 1 || page2[0].ID != "u1" {
		t.Errorf("page2: got %v want [u1]", idList(page2))
	}
}

func TestUsageEventStore_Aggregate(t *testing.T) {
	db := openMem(t)
	store := NewUsageEventStore(db)
	ctx := context.Background()

	// Seed fixture:
	//   day 07-16, model "a": 2 completed rows charged 100 + 200
	//   day 07-16, model "b": 1 completed row  charged 300
	//   day 07-17, model "a": 1 failed row     charged 400
	day16 := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	day17 := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	seed := []*domain.UsageEvent{
		{ID: "a1", TS: day16, GroupID: "g", AccountID: "a", ModelAlias: "a", JST: "x", MediaType: "image",
			ActualCreditsHundredths: 100, ChargedCreditsHundredths: 100, MarkupPct: 1.0,
			Status: domain.JobCompleted, HiggsgoJobID: "j1",
			BillingMonth: "2026-07", BillingDay: "2026-07-16"},
		{ID: "a2", TS: day16, GroupID: "g", AccountID: "a", ModelAlias: "a", JST: "x", MediaType: "image",
			ActualCreditsHundredths: 200, ChargedCreditsHundredths: 200, MarkupPct: 1.0,
			Status: domain.JobCompleted, HiggsgoJobID: "j2",
			BillingMonth: "2026-07", BillingDay: "2026-07-16"},
		{ID: "b1", TS: day16, GroupID: "g", AccountID: "a", ModelAlias: "b", JST: "x", MediaType: "image",
			ActualCreditsHundredths: 300, ChargedCreditsHundredths: 300, MarkupPct: 1.0,
			Status: domain.JobCompleted, HiggsgoJobID: "j3",
			BillingMonth: "2026-07", BillingDay: "2026-07-16"},
		{ID: "a3", TS: day17, GroupID: "g", AccountID: "a", ModelAlias: "a", JST: "x", MediaType: "image",
			ActualCreditsHundredths: 400, ChargedCreditsHundredths: 400, MarkupPct: 1.0,
			Status: domain.JobFailed, HiggsgoJobID: "j4",
			BillingMonth: "2026-07", BillingDay: "2026-07-17"},
	}
	for _, ev := range seed {
		if err := store.Insert(ctx, ev); err != nil {
			t.Fatalf("seed insert %s: %v", ev.ID, err)
		}
	}

	// GroupBy = billing_day: two rows keyed by day, sums as fixture describes.
	rows, err := store.Aggregate(ctx, ports.UsageAggQuery{GroupBy: []string{"billing_day"}})
	if err != nil {
		t.Fatalf("aggregate by day: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("by day: got %d rows want 2", len(rows))
	}
	byDay := map[string]ports.UsageAggRow{}
	for _, r := range rows {
		byDay[r.Keys["billing_day"]] = r
	}
	if r := byDay["2026-07-16"]; r.RequestCount != 3 || r.ChargedCreditsHundredths != 600 || r.CompletedCount != 3 {
		t.Errorf("2026-07-16 row wrong: %+v", r)
	}
	if r := byDay["2026-07-17"]; r.RequestCount != 1 || r.ChargedCreditsHundredths != 400 || r.FailedCount != 1 {
		t.Errorf("2026-07-17 row wrong: %+v", r)
	}

	// GroupBy = model_alias: two rows keyed by model, counts + sums intact.
	rows, err = store.Aggregate(ctx, ports.UsageAggQuery{GroupBy: []string{"model_alias"}})
	if err != nil {
		t.Fatalf("aggregate by model: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("by model: got %d rows want 2", len(rows))
	}
	byModel := map[string]ports.UsageAggRow{}
	for _, r := range rows {
		byModel[r.Keys["model_alias"]] = r
	}
	if r := byModel["a"]; r.RequestCount != 3 || r.ChargedCreditsHundredths != 700 || r.CompletedCount != 2 || r.FailedCount != 1 {
		t.Errorf("model=a row wrong: %+v", r)
	}
	if r := byModel["b"]; r.RequestCount != 1 || r.ChargedCreditsHundredths != 300 || r.CompletedCount != 1 {
		t.Errorf("model=b row wrong: %+v", r)
	}

	// Illegal GroupBy column ("password") must be silently dropped. When
	// combined with a legal column, only the legal one shapes the output.
	rows, err = store.Aggregate(ctx, ports.UsageAggQuery{GroupBy: []string{"password", "model_alias"}})
	if err != nil {
		t.Fatalf("aggregate with illegal col: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("illegal+legal cols: got %d rows want 2", len(rows))
	}
	for _, r := range rows {
		if _, present := r.Keys["password"]; present {
			t.Errorf("illegal col leaked into keys: %+v", r.Keys)
		}
		if _, present := r.Keys["model_alias"]; !present {
			t.Errorf("legal col missing from keys: %+v", r.Keys)
		}
	}

	// GroupBy = billing_hour: synthetic bucket derived from ts via strftime.
	// The fixture puts three rows at 12:00 UTC on 07-16 and one row at
	// 12:00 UTC on 07-17, so we expect two hour buckets keyed by RFC3339
	// hour boundaries. Verifies the derived column is spelled correctly
	// and that the alias hits the Keys map under "billing_hour".
	rows, err = store.Aggregate(ctx, ports.UsageAggQuery{GroupBy: []string{"billing_hour"}})
	if err != nil {
		t.Fatalf("aggregate by hour: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("by hour: got %d rows want 2", len(rows))
	}
	byHour := map[string]ports.UsageAggRow{}
	for _, r := range rows {
		byHour[r.Keys["billing_hour"]] = r
	}
	if r := byHour["2026-07-16T12:00:00Z"]; r.RequestCount != 3 || r.ChargedCreditsHundredths != 600 {
		t.Errorf("hour 2026-07-16T12 wrong: %+v", r)
	}
	if r := byHour["2026-07-17T12:00:00Z"]; r.RequestCount != 1 || r.ChargedCreditsHundredths != 400 {
		t.Errorf("hour 2026-07-17T12 wrong: %+v", r)
	}

	// Only illegal columns → behaves like no group-by: a single roll-up row.
	rows, err = store.Aggregate(ctx, ports.UsageAggQuery{GroupBy: []string{"password"}})
	if err != nil {
		t.Fatalf("aggregate illegal only: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("illegal only: got %d rows want 1", len(rows))
	}
	if rows[0].RequestCount != 4 || rows[0].ChargedCreditsHundredths != 1000 {
		t.Errorf("roll-up wrong: %+v", rows[0])
	}
}

func TestUsageEventStore_QueryEmpty(t *testing.T) {
	db := openMem(t)
	store := NewUsageEventStore(db)
	ctx := context.Background()

	rows, err := store.Query(ctx, ports.UsageQuery{})
	if err != nil {
		t.Fatalf("query empty: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}

	// Aggregate on an empty table returns an empty slice too.
	aggs, err := store.Aggregate(ctx, ports.UsageAggQuery{GroupBy: []string{"model_alias"}})
	if err != nil {
		t.Fatalf("aggregate empty: %v", err)
	}
	if len(aggs) != 0 {
		t.Errorf("expected 0 agg rows, got %d", len(aggs))
	}

	// Regression: rollup (no GROUP BY) on an empty table must return one
	// row of zeros. Previously SUM(CASE WHEN…) collapsed to NULL and the
	// int64 scan targets rejected the value — the /admin/usage/aggregate
	// caller then got a 500 "converting NULL to int64 is unsupported".
	aggs, err = store.Aggregate(ctx, ports.UsageAggQuery{})
	if err != nil {
		t.Fatalf("aggregate empty rollup: %v", err)
	}
	if len(aggs) != 1 {
		t.Fatalf("empty rollup: got %d rows want 1", len(aggs))
	}
	r := aggs[0]
	if r.RequestCount != 0 || r.CompletedCount != 0 || r.FailedCount != 0 ||
		r.RefundedCount != 0 || r.TotalCreditsHundredths != 0 ||
		r.ChargedCreditsHundredths != 0 || r.AvgLatencyMS != 0 {
		t.Errorf("empty rollup row should be all zeros: %+v", r)
	}
}

// idList extracts IDs for concise assertion messages.
func idList(events []domain.UsageEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}
