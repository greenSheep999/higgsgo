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
		RegisteredAt: now, ImportedAt: now,
	}))

	// Seed two activations, in reverse alphabetical order to verify the
	// List helper sorts by bundle_type ASC.
	acts := []domain.UnlimActivation{
		{
			BundleType: "nano_banana_2_4k",
			JobSetType: "nano_banana_pro_unlimited",
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
