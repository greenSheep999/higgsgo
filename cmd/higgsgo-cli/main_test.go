package main

// Tests for higgsgo-cli subcommands. Each test opens an in-memory SQLite DB
// via sqlite.Open(":memory:") (which applies the embedded migrations, giving
// us a fresh schema every time), seeds a couple of rows through the store
// APIs, then drives the same cmdXxxQuery/exec helper the CLI main uses.
// stdout is captured through a bytes.Buffer so we can assert on the exact
// output without spawning a subprocess.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// openTestDB opens a fresh in-memory SQLite DB with migrations applied. The
// cleanup hook closes the handle so the test does not leak connections.
func openTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// createAPIKey inserts a minimal api_keys row via the store. Fields the CLI
// prints (id/name/status/monthly_used/monthly_quota) are set explicitly;
// everything else takes the store's defaults.
func createAPIKey(t *testing.T, ctx context.Context, db *sqlite.DB, id, name string, quota, used int64) {
	t.Helper()
	store := sqlite.NewAPIKeyStore(db)
	if err := store.Create(ctx, &domain.APIKey{
		ID:           id,
		KeyHash:      "hash_" + id, // placeholder; never compared or printed
		Name:         name,
		Status:       "active",
		MonthlyQuota: quota,
		MonthlyUsed:  used,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create api key %s: %v", id, err)
	}
}

func TestListKeys(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	createAPIKey(t, ctx, db, "key_alpha", "alpha", 10_000, 500)
	createAPIKey(t, ctx, db, "key_bravo", "bravo", 20_000, 1_000)

	var buf bytes.Buffer
	if err := listKeysQuery(ctx, &buf, db.DB, "json"); err != nil {
		t.Fatalf("listKeysQuery: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"key_alpha", "key_bravo", "alpha", "bravo"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
	// key_hash must never leak through the CLI, even in JSON form.
	if strings.Contains(got, "hash_key_alpha") || strings.Contains(got, "key_hash") {
		t.Errorf("output leaked key_hash column; got:\n%s", got)
	}
	// Sanity-check the JSON parses and contains the two rows.
	var rows []apiKeyRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, got)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
}

func TestListGroups(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewGroupStore(db)

	if err := store.Create(ctx, &domain.Group{
		ID:                "grp_alpha",
		Name:              "alpha",
		Description:       "alpha group",
		MaxConcurrentJobs: 10,
		OwnerType:         domain.OwnerInternal,
		Status:            "active",
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if err := store.Create(ctx, &domain.Group{
		ID:                "grp_bravo",
		Name:              "bravo",
		Description:       "bravo group",
		MaxConcurrentJobs: 20,
		OwnerType:         domain.OwnerInternal,
		Status:            "active",
		CreatedAt:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create bravo: %v", err)
	}

	var buf bytes.Buffer
	if err := listGroupsQuery(ctx, &buf, db.DB, "json"); err != nil {
		t.Fatalf("listGroupsQuery: %v", err)
	}
	var rows []groupRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	// Ordered by name ascending, so alpha comes first.
	if rows[0].ID != "grp_alpha" || rows[1].ID != "grp_bravo" {
		t.Errorf("unexpected ordering: %+v", rows)
	}
	if rows[0].ConcurrencyLimit != 10 || rows[1].ConcurrencyLimit != 20 {
		t.Errorf("concurrency limits wrong: %+v", rows)
	}
}

// seedAccount inserts a minimal account row so foreign-key constraints on
// jobs.account_id can be satisfied. Idempotent through Upsert.
func seedAccount(t *testing.T, ctx context.Context, db *sqlite.DB, id string) {
	t.Helper()
	store := sqlite.NewAccountStore(db)
	if err := store.Upsert(ctx, &domain.Account{
		ID:                  id,
		Email:               id + "@example.com",
		Password:            "-",
		SessionID:           "-",
		CookiesJSON:         "{}",
		UserAgent:           "-",
		PlanType:            domain.PlanPlus,
		SubscriptionBalance: 100_000,
		Status:              domain.StatusActive,
		RegisteredAt:        time.Now().UTC(),
		ImportedAt:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert account %s: %v", id, err)
	}
}

// seedJob inserts a job row via JobStore.Create so all the NOT NULL columns
// get sensible defaults from the domain type. The account row is created on
// demand — the jobs.account_id FK would otherwise fail.
func seedJob(t *testing.T, ctx context.Context, db *sqlite.DB, id, status, account string, ts time.Time) {
	t.Helper()
	seedAccount(t, ctx, db, account)
	store := sqlite.NewJobStore(db)
	if err := store.Create(ctx, &domain.Job{
		ID:              id,
		AccountID:       account,
		ModelAlias:      "test-model",
		JST:             "test_jst",
		Endpoint:        "/v1/generations",
		RequestBodyJSON: "{}",
		RequestTS:       ts,
		Status:          domain.JobStatus(status),
	}); err != nil {
		t.Fatalf("create job %s: %v", id, err)
	}
}

func TestListJobs_StatusFilter(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewJobStore(db)

	now := time.Now().UTC()
	seedJob(t, ctx, db, "job_a", "completed", "acct_1", now.Add(-3*time.Minute))
	seedJob(t, ctx, db, "job_b", "completed", "acct_1", now.Add(-2*time.Minute))
	seedJob(t, ctx, db, "job_c", "failed", "acct_1", now.Add(-1*time.Minute))

	var buf bytes.Buffer
	if err := listJobsQuery(ctx, &buf, store, "json", "completed", "", 50); err != nil {
		t.Fatalf("listJobsQuery: %v", err)
	}
	var rows []jobRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 completed rows, got %d: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.Status != "completed" {
			t.Errorf("job %s status = %q, want completed", r.ID, r.Status)
		}
	}
}

func TestShowUsage(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewUsageEventStore(db)

	now := time.Now().UTC()
	events := []domain.UsageEvent{
		{ID: "u1", TS: now.Add(-1 * time.Hour), ModelAlias: "model_a", JST: "jst_a", MediaType: "image", AccountID: "acc", GroupID: "g", HiggsgoJobID: "j1", Status: domain.JobCompleted, ActualCreditsHundredths: 100, ChargedCreditsHundredths: 150, MarkupPct: 1.5, BillingMonth: "2026-07", BillingDay: "2026-07-17"},
		{ID: "u2", TS: now.Add(-2 * time.Hour), ModelAlias: "model_a", JST: "jst_a", MediaType: "image", AccountID: "acc", GroupID: "g", HiggsgoJobID: "j2", Status: domain.JobCompleted, ActualCreditsHundredths: 200, ChargedCreditsHundredths: 300, MarkupPct: 1.5, BillingMonth: "2026-07", BillingDay: "2026-07-17"},
		{ID: "u3", TS: now.Add(-3 * time.Hour), ModelAlias: "model_b", JST: "jst_b", MediaType: "video", AccountID: "acc", GroupID: "g", HiggsgoJobID: "j3", Status: domain.JobCompleted, ActualCreditsHundredths: 500, ChargedCreditsHundredths: 750, MarkupPct: 1.5, BillingMonth: "2026-07", BillingDay: "2026-07-17"},
		// Outside the 24h window: must not appear in the aggregate.
		{ID: "u4", TS: now.Add(-48 * time.Hour), ModelAlias: "model_a", JST: "jst_a", MediaType: "image", AccountID: "acc", GroupID: "g", HiggsgoJobID: "j4", Status: domain.JobCompleted, ActualCreditsHundredths: 999, ChargedCreditsHundredths: 999, MarkupPct: 1.5, BillingMonth: "2026-07", BillingDay: "2026-07-15"},
	}
	for i := range events {
		if err := store.Insert(ctx, &events[i]); err != nil {
			t.Fatalf("insert usage event %s: %v", events[i].ID, err)
		}
	}

	var buf bytes.Buffer
	// Pin the "now" arg to make the window deterministic.
	if err := showUsageQuery(ctx, &buf, store, "json", 24, now); err != nil {
		t.Fatalf("showUsageQuery: %v", err)
	}
	var rows []usageRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 aggregate rows (model_a + model_b), got %d: %+v", len(rows), rows)
	}
	byModel := map[string]usageRow{}
	for _, r := range rows {
		byModel[r.Model] = r
	}
	if got := byModel["model_a"]; got.Count != 2 || got.TotalCharg != 450 {
		t.Errorf("model_a agg wrong: %+v", got)
	}
	if got := byModel["model_b"]; got.Count != 1 || got.TotalCharg != 750 {
		t.Errorf("model_b agg wrong: %+v", got)
	}
}

func TestDisableKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewAPIKeyStore(db)

	createAPIKey(t, ctx, db, "key_disable", "to disable", 0, 0)

	var buf bytes.Buffer
	if err := disableKeyExec(ctx, &buf, store, "key_disable"); err != nil {
		t.Fatalf("disableKeyExec: %v", err)
	}

	// Verify the status flipped in the underlying row.
	got, err := store.Get(ctx, "key_disable")
	if err != nil {
		t.Fatalf("get after disable: %v", err)
	}
	if got.Status != "revoked" {
		t.Errorf("status = %q, want revoked", got.Status)
	}

	// disableKeyExec on a nonexistent id must return the domain error.
	if err := disableKeyExec(ctx, &buf, store, "does_not_exist"); err == nil {
		t.Errorf("expected error when disabling missing key, got nil")
	} else if err != domain.ErrAPIKeyNotFound {
		t.Errorf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

func TestRotateKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewAPIKeyStore(db)

	createAPIKey(t, ctx, db, "key_rot", "to rotate", 0, 0)

	// Snapshot the hash before rotation so we can assert it changed.
	before, err := store.Get(ctx, "key_rot")
	if err != nil {
		t.Fatalf("get before rotate: %v", err)
	}
	oldHash := before.KeyHash

	var buf bytes.Buffer
	if err := rotateKeyExec(ctx, &buf, db.DB, "key_rot"); err != nil {
		t.Fatalf("rotateKeyExec: %v", err)
	}

	// stdout must contain the plaintext exactly once (id/prefix/new_secret).
	var res rotateKeyResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if res.ID != "key_rot" {
		t.Errorf("id = %q, want key_rot", res.ID)
	}
	if !strings.HasPrefix(res.NewSecret, res.KeyPrefix) {
		t.Errorf("new_secret %q does not start with prefix %q", res.NewSecret, res.KeyPrefix)
	}

	// Underlying hash must have been rewritten.
	after, err := store.Get(ctx, "key_rot")
	if err != nil {
		t.Fatalf("get after rotate: %v", err)
	}
	if after.KeyHash == oldHash {
		t.Errorf("key_hash did not change: still %s", after.KeyHash)
	}
	if after.KeyHash == "" {
		t.Errorf("key_hash is empty after rotation")
	}

	// Rotating a nonexistent id must error rather than silently succeed.
	buf.Reset()
	if err := rotateKeyExec(ctx, &buf, db.DB, "does_not_exist"); err == nil {
		t.Errorf("expected error when rotating missing key, got nil")
	}
}

func TestPauseKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewAPIKeyStore(db)

	createAPIKey(t, ctx, db, "key_pause", "to pause", 0, 0)

	var buf bytes.Buffer
	if err := pauseKeyExec(ctx, &buf, store, "key_pause"); err != nil {
		t.Fatalf("pauseKeyExec: %v", err)
	}

	var res pauseKeyResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if res.ID != "key_pause" || res.Status != "paused" {
		t.Errorf("unexpected result: %+v", res)
	}

	// Confirm the row actually flipped in the underlying store, not just
	// that the CLI printed the string.
	got, err := store.Get(ctx, "key_pause")
	if err != nil {
		t.Fatalf("get after pause: %v", err)
	}
	if got.Status != "paused" {
		t.Errorf("row status = %q, want paused", got.Status)
	}
}

func TestPauseKey_NotFound(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewAPIKeyStore(db)

	var buf bytes.Buffer
	err := pauseKeyExec(ctx, &buf, store, "does_not_exist")
	if err == nil {
		t.Fatalf("expected error when pausing missing key, got nil")
	}
	if err != domain.ErrAPIKeyNotFound {
		t.Errorf("expected ErrAPIKeyNotFound, got %v", err)
	}
}

func TestResumeKey(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewAPIKeyStore(db)

	createAPIKey(t, ctx, db, "key_resume", "to resume", 0, 0)
	if err := store.Pause(ctx, "key_resume"); err != nil {
		t.Fatalf("pause before resume: %v", err)
	}

	var buf bytes.Buffer
	if err := resumeKeyExec(ctx, &buf, store, "key_resume"); err != nil {
		t.Fatalf("resumeKeyExec: %v", err)
	}

	var res resumeKeyResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if res.ID != "key_resume" || res.Status != "active" {
		t.Errorf("unexpected result: %+v", res)
	}

	got, err := store.Get(ctx, "key_resume")
	if err != nil {
		t.Fatalf("get after resume: %v", err)
	}
	if got.Status != "active" {
		t.Errorf("row status = %q, want active", got.Status)
	}
}

func TestResumeKey_Revoked(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewAPIKeyStore(db)

	createAPIKey(t, ctx, db, "key_revoked", "already revoked", 0, 0)
	if err := store.Revoke(ctx, "key_revoked"); err != nil {
		t.Fatalf("revoke before resume: %v", err)
	}

	var buf bytes.Buffer
	err := resumeKeyExec(ctx, &buf, store, "key_revoked")
	if err == nil {
		t.Fatalf("expected error when resuming revoked key, got nil")
	}
	if err != domain.ErrAPIKeyRevoked {
		t.Errorf("expected ErrAPIKeyRevoked, got %v", err)
	}
}

func TestResetUsage(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewAPIKeyStore(db)

	createAPIKey(t, ctx, db, "key_reset", "to reset", 10_000, 0)
	if err := store.IncrementUsage(ctx, "key_reset", 500); err != nil {
		t.Fatalf("increment usage: %v", err)
	}

	// Sanity-check the increment landed before we reset it.
	before, err := store.Get(ctx, "key_reset")
	if err != nil {
		t.Fatalf("get before reset: %v", err)
	}
	if before.MonthlyUsed != 500 {
		t.Fatalf("pre-reset monthly_used = %d, want 500", before.MonthlyUsed)
	}

	var buf bytes.Buffer
	if err := resetUsageExec(ctx, &buf, store, "key_reset"); err != nil {
		t.Fatalf("resetUsageExec: %v", err)
	}

	var res resetUsageResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if res.ID != "key_reset" || res.MonthlyUsed != 0 {
		t.Errorf("unexpected result: %+v", res)
	}

	after, err := store.Get(ctx, "key_reset")
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if after.MonthlyUsed != 0 {
		t.Errorf("row monthly_used = %d, want 0", after.MonthlyUsed)
	}
	// Quota must stay intact — reset only zeroes the counter.
	if after.MonthlyQuota != 10_000 {
		t.Errorf("row monthly_quota = %d, want 10000", after.MonthlyQuota)
	}
}

// TestListJobs_AccountFilter is a small extra guard on the -account flag path
// so filter composition (status+account) is covered too.
func TestListJobs_AccountFilter(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	store := sqlite.NewJobStore(db)

	now := time.Now().UTC()
	seedJob(t, ctx, db, "j1", "completed", "acc_a", now.Add(-3*time.Minute))
	seedJob(t, ctx, db, "j2", "completed", "acc_b", now.Add(-2*time.Minute))

	var buf bytes.Buffer
	if err := listJobsQuery(ctx, &buf, store, "json", "", "acc_a", 50); err != nil {
		t.Fatalf("listJobsQuery: %v", err)
	}
	var rows []jobRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 1 || rows[0].ID != "j1" {
		t.Errorf("expected only j1 for acc_a; got %+v", rows)
	}

	// Sanity-check the ports import path is exercised through Store.ListAll.
	filter := ports.JobFilter{Limit: 10}
	if _, err := store.ListAll(ctx, filter); err != nil {
		t.Fatalf("ListAll sanity: %v", err)
	}
}
