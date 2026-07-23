package sqlite

// Tests for the load-balance router's prefer_unlim + prefer_free_quota
// hooks on PickAndLock. Four scenarios covering the two flags:
//
//   1. PreferUnlim on with a matching activation — bundle holder wins
//      over an otherwise-identical account.
//   2. PreferUnlim on but the sole activation is expired — the bundle
//      holder loses (expiry filter works).
//   3. PreferFreeQuota on with a positive column — quota holder wins.
//   4. UnlimActivations CRUD — Replace + List round-trips cleanly and
//      the deletion path clears stale rows.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestAccountStore_UnlimActivations_ReplaceAndList checks the CRUD path:
// Replace with a two-row set, List reads them back sorted by bundle_type,
// then Replace with an empty slice clears the whole set.
func TestAccountStore_UnlimActivations_ReplaceAndList(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_holder", Email: "holder@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusActive,
		SubscriptionBalance: 100000,
		RegisteredAt:        now, ImportedAt: now,
	}))

	// Seed two activations, in reverse alphabetical order to verify the
	// List helper sorts by bundle_type ASC.
	acts := []domain.UnlimActivation{
		{
			BundleType:  "nano_banana_2_4k",
			JobSetType:  "nano_banana_pro_unlimited",
			Resolutions: []string{"1k", "2k", "4k"},
			ActivatedAt: now.Add(-1 * time.Hour),
		},
		{
			BundleType:  "kling_3_4k",
			JobSetType:  "kling_3_unlimited",
			Resolutions: []string{"720p", "1080p", "4k"},
			ExpiresAt:   now.Add(24 * time.Hour),
			ActivatedAt: now.Add(-2 * time.Hour),
		},
	}
	must(store.ReplaceUnlimActivations(ctx, "acc_holder", acts))

	got, err := store.ListUnlimActivations(ctx, "acc_holder")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 activations, got %d", len(got))
	}
	// Ordered by bundle_type ASC — kling_3_4k comes first.
	if got[0].BundleType != "kling_3_4k" {
		t.Errorf("row 0 bundle_type: got %q want kling_3_4k", got[0].BundleType)
	}
	if got[0].JobSetType != "kling_3_unlimited" {
		t.Errorf("row 0 job_set_type: got %q", got[0].JobSetType)
	}
	if len(got[0].Resolutions) != 3 || got[0].Resolutions[0] != "720p" {
		t.Errorf("row 0 resolutions round-trip: %+v", got[0].Resolutions)
	}
	if got[0].ExpiresAt.IsZero() {
		t.Errorf("row 0 expires_at zero, want %v", now.Add(24*time.Hour))
	}
	if got[1].BundleType != "nano_banana_2_4k" {
		t.Errorf("row 1 bundle_type: got %q", got[1].BundleType)
	}
	if !got[1].ExpiresAt.IsZero() {
		t.Errorf("row 1 expires_at should be zero (permanent), got %v", got[1].ExpiresAt)
	}

	// Replace with empty slice clears the set — the row deletes cascade
	// nothing external, but the join in PickAndLock stops finding matches.
	must(store.ReplaceUnlimActivations(ctx, "acc_holder", nil))
	got, err = store.ListUnlimActivations(ctx, "acc_holder")
	if err != nil {
		t.Fatalf("list after empty replace: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 activations after empty replace, got %d", len(got))
	}
}

// TestAccountStore_PickAndLock_PreferUnlim_HitBundleHolder seeds two
// otherwise-identical accounts with a bundle holder for the requested
// job_set_type and asserts the router picks the holder. Without the
// flag the two rows would tie on LRU/in_flight and RANDOM() would pick
// arbitrarily; with the flag, the holder always wins.
func TestAccountStore_PickAndLock_PreferUnlim_HitBundleHolder(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_no_bundle", Email: "nobundle@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: now.Add(-time.Hour),
		RegisteredAt: now, ImportedAt: now,
	}))
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_holder", Email: "holder@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: now.Add(-time.Hour),
		RegisteredAt: now, ImportedAt: now,
	}))

	must(store.ReplaceUnlimActivations(ctx, "acc_holder", []domain.UnlimActivation{
		{
			BundleType:  "nano_banana_2_4k",
			JobSetType:  "nano_banana_pro_unlimited",
			ActivatedAt: now.Add(-time.Hour),
		},
	}))

	got, tok, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		RouteStrategy:     domain.RouteRoundRobin,
		UnlimJobSetType:   "nano_banana_pro_unlimited",
		LoadBalance: ports.LoadBalanceOpts{
			Populated:          true,
			TierAware:          true,
			PreferUnlim:        true,
			BalanceHeadroomPct: 120,
			Jitter:             false,
		},
	})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.ID != "acc_holder" {
		t.Errorf("prefer_unlim: got %s want acc_holder", got.ID)
	}
	must(store.Unlock(ctx, got.ID, tok))
}

// TestAccountStore_PickAndLock_PreferUnlim_ExpiredBundleIgnored seeds a
// holder whose bundle expired an hour ago and asserts the router does
// not treat it as a bundle holder. Because both rows are otherwise
// identical, expiry pushing the "holder" out of the EXISTS subquery
// should make the pick indistinguishable from the flag-off case.
//
// We test the negative by giving acc_no_bundle a fresher LRU stamp:
// after the EXISTS filter drops the expired holder from rank 0, the
// remaining tail is `LRU ASC` — the older LRU should win. If the
// expiry filter were broken the holder would still sort first.
func TestAccountStore_PickAndLock_PreferUnlim_ExpiredBundleIgnored(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_no_bundle", Email: "nobundle@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: now.Add(-24 * time.Hour), // older LRU
		RegisteredAt: now, ImportedAt: now,
	}))
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_expired_holder", Email: "expired@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: now.Add(-1 * time.Minute), // fresh LRU
		RegisteredAt: now, ImportedAt: now,
	}))

	must(store.ReplaceUnlimActivations(ctx, "acc_expired_holder", []domain.UnlimActivation{
		{
			BundleType:  "nano_banana_2_4k",
			JobSetType:  "nano_banana_pro_unlimited",
			ExpiresAt:   now.Add(-1 * time.Hour), // expired an hour ago
			ActivatedAt: now.Add(-48 * time.Hour),
		},
	}))

	got, tok, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		RouteStrategy:     domain.RouteRoundRobin,
		UnlimJobSetType:   "nano_banana_pro_unlimited",
		LoadBalance: ports.LoadBalanceOpts{
			Populated:          true,
			TierAware:          true,
			PreferUnlim:        true,
			BalanceHeadroomPct: 120,
			Jitter:             false,
		},
	})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	// Expired holder should NOT be treated as a bundle holder; older-LRU
	// acc_no_bundle wins on the LRU tiebreaker.
	if got.ID != "acc_no_bundle" {
		t.Errorf("prefer_unlim with expired bundle: got %s want acc_no_bundle (expired activation should be ignored)", got.ID)
	}
	must(store.Unlock(ctx, got.ID, tok))
}

// TestAccountStore_PickAndLock_PreferFreeQuota_HitQuotaHolder seeds
// two accounts, one with face_swap_credits > 0, and asserts the
// router prefers the quota holder when the flag is on. Mirrors the
// PreferUnlim test structure — same LRU tie, quota holder pulled to
// rank 0 by the CASE.
func TestAccountStore_PickAndLock_PreferFreeQuota_HitQuotaHolder(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_no_quota", Email: "noquota@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: now.Add(-time.Hour),
		RegisteredAt: now, ImportedAt: now,
	}))
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_quota_holder", Email: "quota@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: now.Add(-time.Hour),
		RegisteredAt: now, ImportedAt: now,
	}))
	// Only acc_quota_holder gets a positive quota value — mimics the
	// refresher having written this row on a plan that grants
	// face_swap_credits (starter grants 2.0 per the sample /user response).
	must(store.UpdateFreeQuota(ctx, "acc_quota_holder", domain.FreeQuotaCounters{
		FaceSwapCredits: 2.0,
	}))

	got, tok, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		RouteStrategy:     domain.RouteRoundRobin,
		FreeQuotaField:    "face_swap_credits",
		LoadBalance: ports.LoadBalanceOpts{
			Populated:          true,
			TierAware:          true,
			PreferFreeQuota:    true,
			BalanceHeadroomPct: 120,
			Jitter:             false,
		},
	})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.ID != "acc_quota_holder" {
		t.Errorf("prefer_free_quota: got %s want acc_quota_holder", got.ID)
	}
	must(store.Unlock(ctx, got.ID, tok))

	// Fractional grant edge case: qwen_camera_control_credits=0.4 is
	// strictly > 0 and must still qualify (starter gets this exact
	// value from the sample /user response). Reset LRU on the winner
	// so the pick condition is not skewed by the previous run.
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_quota_holder", Email: "quota@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, LastUsedAt: now.Add(-time.Hour),
		RegisteredAt: now, ImportedAt: now,
	}))
	must(store.UpdateFreeQuota(ctx, "acc_quota_holder", domain.FreeQuotaCounters{
		QwenCameraControlCredits: 0.4,
	}))
	got2, tok2, err := store.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		RouteStrategy:     domain.RouteRoundRobin,
		FreeQuotaField:    "qwen_camera_control_credits",
		LoadBalance: ports.LoadBalanceOpts{
			Populated:          true,
			TierAware:          true,
			PreferFreeQuota:    true,
			BalanceHeadroomPct: 120,
			Jitter:             false,
		},
	})
	if err != nil {
		t.Fatalf("pick fractional: %v", err)
	}
	if got2.ID != "acc_quota_holder" {
		t.Errorf("prefer_free_quota fractional: got %s want acc_quota_holder", got2.ID)
	}
	must(store.Unlock(ctx, got2.ID, tok2))
}

// TestAccountStore_HasActiveUnlimFor covers the standalone predicate the
// proxy service uses to decide whether to route a request to the
// `_unlimited` variant endpoint. It mirrors the expiry semantics of the
// prefer_unlim sort hint: NULL/empty expires_at means "never expires",
// a future timestamp is active, a past one is not, and the job_set_type
// must match exactly.
func TestAccountStore_HasActiveUnlimFor(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_u", Email: "u@x", Password: "-",
		SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusActive,
		SubscriptionBalance: 100000,
		RegisteredAt:        now, ImportedAt: now,
	}))

	// Three activations on one account:
	//   - never-expiring seedance bundle (zero-value ExpiresAt → stored NULL)
	//   - future-dated kling bundle
	//   - already-expired gpt-image bundle
	must(store.ReplaceUnlimActivations(ctx, "acc_u", []domain.UnlimActivation{
		{
			BundleType:  "seedance_2_720p",
			JobSetType:  "seedance_2_unlimited",
			ActivatedAt: now.Add(-1 * time.Hour),
		},
		{
			BundleType:  "kling_3_4k",
			JobSetType:  "kling_3_unlimited",
			ExpiresAt:   now.Add(48 * time.Hour),
			ActivatedAt: now.Add(-2 * time.Hour),
		},
		{
			BundleType:  "gpt_image_2_hd",
			JobSetType:  "gpt_image_2_unlimited",
			ExpiresAt:   now.Add(-1 * time.Hour),
			ActivatedAt: now.Add(-72 * time.Hour),
		},
	}))

	cases := []struct {
		name       string
		accountID  string
		jst        string
		wantActive bool
	}{
		{"never-expires bundle is active", "acc_u", "seedance_2_unlimited", true},
		{"future-dated bundle is active", "acc_u", "kling_3_unlimited", true},
		{"expired bundle is inactive", "acc_u", "gpt_image_2_unlimited", false},
		{"unknown jst is inactive", "acc_u", "veo_3_unlimited", false},
		{"unknown account is inactive", "acc_missing", "seedance_2_unlimited", false},
		{"empty jst returns false", "acc_u", "", false},
		{"empty account returns false", "", "seedance_2_unlimited", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.HasActiveUnlimFor(ctx, tc.accountID, tc.jst)
			if err != nil {
				t.Fatalf("HasActiveUnlimFor: %v", err)
			}
			if got != tc.wantActive {
				t.Errorf("got %v want %v", got, tc.wantActive)
			}
		})
	}
}

// TestAccountStore_CountActiveUnlimByJST checks the replenish S1 grouping:
// only active accounts + non-expired bundles count, grouped by
// job_set_type, distinct per account.
func TestAccountStore_CountActiveUnlimByJST(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)

	// Two active accounts + one suspended account.
	for _, id := range []string{"act_1", "act_2"} {
		must(store.Upsert(ctx, &domain.Account{
			ID: id, Email: id + "@x", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
			PlanType: domain.PlanPlus, Status: domain.StatusActive, SubscriptionBalance: 100000,
			RegisteredAt: now, ImportedAt: now,
		}))
	}
	must(store.Upsert(ctx, &domain.Account{
		ID: "susp", Email: "susp@x", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusSuspended, SubscriptionBalance: 100000,
		RegisteredAt: now, ImportedAt: now,
	}))

	// act_1 + act_2 hold live seedance; act_2 also holds an EXPIRED kling;
	// susp holds live seedance but is not active.
	must(store.ReplaceUnlimActivations(ctx, "act_1", []domain.UnlimActivation{
		{BundleType: "seedance_2_720p", JobSetType: "seedance_2_unlimited", ActivatedAt: now},
	}))
	must(store.ReplaceUnlimActivations(ctx, "act_2", []domain.UnlimActivation{
		{BundleType: "seedance_2_720p", JobSetType: "seedance_2_unlimited", ActivatedAt: now},
		{BundleType: "kling_3_4k", JobSetType: "kling_3_unlimited", ExpiresAt: now.Add(-time.Hour), ActivatedAt: now},
	}))
	must(store.ReplaceUnlimActivations(ctx, "susp", []domain.UnlimActivation{
		{BundleType: "seedance_2_720p", JobSetType: "seedance_2_unlimited", ActivatedAt: now},
	}))

	got, err := store.CountActiveUnlimByJST(ctx)
	if err != nil {
		t.Fatalf("CountActiveUnlimByJST: %v", err)
	}
	// seedance: act_1 + act_2 (susp excluded, not active) = 2.
	if got["seedance_2_unlimited"] != 2 {
		t.Errorf("seedance count: got %d want 2 (%v)", got["seedance_2_unlimited"], got)
	}
	// kling: act_2's only kling bundle is expired → not counted.
	if _, ok := got["kling_3_unlimited"]; ok {
		t.Errorf("expired kling should not appear: %v", got)
	}
}

// TestAccountStore_UpdateUpstreamStatusAndGrace round-trips the migration
// 021 derived columns: UpdateUpstreamStatus writes blocked/suspended/
// paused, UpdateGraceStatus writes grace_status, and neither touches
// status or fail_streak.
func TestAccountStore_UpdateUpstreamStatusAndGrace(t *testing.T) {
	db := openMem(t)
	store := NewAccountStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	must(store.Upsert(ctx, &domain.Account{
		ID: "acc_s", Email: "s@x", Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, Status: domain.StatusActive, FailStreak: 2,
		RegisteredAt: now, ImportedAt: now,
	}))

	must(store.UpdateUpstreamStatus(ctx, "acc_s", ports.UpstreamStatusUpdate{
		BlockedAt: "2026-07-20T00:00:00Z", SuspendedAt: "", IsPaused: true,
	}))
	must(store.UpdateGraceStatus(ctx, "acc_s", "grace"))

	got, err := store.Get(ctx, "acc_s")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BlockedAt != "2026-07-20T00:00:00Z" {
		t.Errorf("blocked_at: got %q", got.BlockedAt)
	}
	if !got.IsPaused {
		t.Errorf("is_paused: got false want true")
	}
	if got.GraceStatus != "grace" {
		t.Errorf("grace_status: got %q want grace", got.GraceStatus)
	}
	// Derived writes must not disturb lifecycle fields.
	if got.Status != domain.StatusActive {
		t.Errorf("status changed: got %q want active", got.Status)
	}
	if got.FailStreak != 2 {
		t.Errorf("fail_streak changed: got %d want 2", got.FailStreak)
	}
}
