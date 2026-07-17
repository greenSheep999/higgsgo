package sqlite

// Tests for ModelHealthStore: append semantics (Insert), Latest resolution
// across multiple probe rows, and idempotency of the (jst, checked_at)
// primary key when the same tuple is re-inserted.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func TestModelHealthStore_InsertAndLatest(t *testing.T) {
	db := openMem(t)
	store := NewModelHealthStore(db)
	ctx := context.Background()

	// Latest on an empty table must return (nil, nil) so callers can
	// distinguish "never checked" from real errors.
	got, err := store.Latest(ctx, "seedance_2_0")
	if err != nil {
		t.Fatalf("latest on empty: %v", err)
	}
	if got != nil {
		t.Fatalf("latest on empty: expected nil, got %+v", got)
	}

	// Two probes at distinct timestamps: the second one must win.
	t1 := time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)
	if err := store.Insert(ctx, "seedance_2_0", t1, domain.JobFailed, 500, 0, 12); err != nil {
		t.Fatalf("insert #1: %v", err)
	}
	if err := store.Insert(ctx, "seedance_2_0", t2, domain.JobCompleted, 200, 1800, 20); err != nil {
		t.Fatalf("insert #2: %v", err)
	}

	got, err = store.Latest(ctx, "seedance_2_0")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got == nil {
		t.Fatalf("latest: expected row, got nil")
	}
	if got.Verdict != domain.JobCompleted {
		t.Errorf("verdict: got %q want completed", got.Verdict)
	}
	if !got.CheckedAt.Equal(t2) {
		t.Errorf("checked_at: got %v want %v", got.CheckedAt, t2)
	}
	if got.HTTPStatus != 200 {
		t.Errorf("http_status: got %d want 200", got.HTTPStatus)
	}
	if got.Cost != 1800 {
		t.Errorf("cost: got %d want 1800", got.Cost)
	}
	if got.PollTimeSec != 20 {
		t.Errorf("poll_time_sec: got %d want 20", got.PollTimeSec)
	}
}

func TestModelHealthStore_InsertIsIdempotent(t *testing.T) {
	db := openMem(t)
	store := NewModelHealthStore(db)
	ctx := context.Background()

	// (jst, checked_at) is the primary key. Re-inserting the same tuple
	// must ON CONFLICT DO UPDATE rather than raise a UNIQUE violation, so
	// a probe that retries after a partial write can just call Insert again.
	ts := time.Date(2026, 7, 17, 6, 0, 0, 0, time.UTC)
	if err := store.Insert(ctx, "veo3_1", ts, domain.JobFailed, 503, 0, 5); err != nil {
		t.Fatalf("insert #1: %v", err)
	}
	if err := store.Insert(ctx, "veo3_1", ts, domain.JobCompleted, 200, 900, 15); err != nil {
		t.Fatalf("insert #2 (should upsert): %v", err)
	}

	got, err := store.Latest(ctx, "veo3_1")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got == nil {
		t.Fatalf("latest: expected row after upsert")
	}
	if got.Verdict != domain.JobCompleted {
		t.Errorf("verdict after upsert: got %q want completed", got.Verdict)
	}
	if got.Cost != 900 {
		t.Errorf("cost after upsert: got %d want 900", got.Cost)
	}

	// Exactly one row should exist for veo3_1 (upsert did not append).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM model_health WHERE jst = ?`, "veo3_1").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if n != 1 {
		t.Fatalf("row count for veo3_1: got %d want 1", n)
	}
}

func TestModelHealthStore_LatestPerJST(t *testing.T) {
	db := openMem(t)
	store := NewModelHealthStore(db)
	ctx := context.Background()

	// Two different JSTs probed on the same day. Latest must scope by jst
	// so cross-model interleaving does not confuse the query.
	t1 := time.Date(2026, 7, 17, 6, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 17, 6, 5, 0, 0, time.UTC)
	if err := store.Insert(ctx, "seedance_2_0", t1, domain.JobCompleted, 200, 1800, 12); err != nil {
		t.Fatalf("insert seedance: %v", err)
	}
	if err := store.Insert(ctx, "veo3_1", t2, domain.JobFailed, 500, 0, 3); err != nil {
		t.Fatalf("insert veo3: %v", err)
	}

	seedance, err := store.Latest(ctx, "seedance_2_0")
	if err != nil || seedance == nil {
		t.Fatalf("latest seedance: %v %+v", err, seedance)
	}
	if seedance.Verdict != domain.JobCompleted {
		t.Errorf("seedance verdict: got %q want completed", seedance.Verdict)
	}

	veo3, err := store.Latest(ctx, "veo3_1")
	if err != nil || veo3 == nil {
		t.Fatalf("latest veo3: %v %+v", err, veo3)
	}
	if veo3.Verdict != domain.JobFailed {
		t.Errorf("veo3 verdict: got %q want failed", veo3.Verdict)
	}
}

func TestModelHealthStore_List_Empty(t *testing.T) {
	db := openMem(t)
	store := NewModelHealthStore(db)
	rows, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("empty table: got %d rows want 0", len(rows))
	}
}

func TestModelHealthStore_List_OrderByCheckedAtDesc(t *testing.T) {
	db := openMem(t)
	store := NewModelHealthStore(db)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	// Insert 3 rows for different jsts with rising checked_at so
	// DESC order is oldest last.
	rows := []struct {
		jst  string
		at   time.Time
		vdc  domain.JobStatus
	}{
		{"seedance_2_0", base.Add(-2 * time.Hour), domain.JobCompleted},
		{"veo3_1", base.Add(-1 * time.Hour), domain.JobFailed},
		{"text2image_soul", base, domain.JobCompleted},
	}
	for _, r := range rows {
		if err := store.Insert(ctx, r.jst, r.at, r.vdc, 200, 0, 1); err != nil {
			t.Fatalf("insert %s: %v", r.jst, err)
		}
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("row count: got %d want 3", len(got))
	}
	want := []string{"text2image_soul", "veo3_1", "seedance_2_0"}
	for i, w := range want {
		if got[i].JST != w {
			t.Errorf("row %d: got %q want %q", i, got[i].JST, w)
		}
	}
}

func TestModelHealthStore_List_MultipleVerdicts(t *testing.T) {
	db := openMem(t)
	store := NewModelHealthStore(db)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	rows := []struct {
		jst string
		vdc domain.JobStatus
	}{
		{"a", domain.JobCompleted},
		{"b", domain.JobFailed},
		{"c", domain.JobPending},
	}
	for i, r := range rows {
		if err := store.Insert(ctx, r.jst, base.Add(time.Duration(i)*time.Minute), r.vdc, 200, 0, 1); err != nil {
			t.Fatalf("insert %s: %v", r.jst, err)
		}
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("row count: got %d want 3", len(got))
	}
	seen := map[domain.JobStatus]bool{}
	for _, r := range got {
		seen[r.Verdict] = true
	}
	for _, want := range []domain.JobStatus{domain.JobCompleted, domain.JobFailed, domain.JobPending} {
		if !seen[want] {
			t.Errorf("verdict %q missing from list", want)
		}
	}
}
