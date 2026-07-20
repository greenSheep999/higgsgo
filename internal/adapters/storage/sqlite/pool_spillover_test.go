package sqlite

// Integration-level test for the P3-10 spillover pattern at the pick
// layer. Not a Service.Generate test (that's covered upstream with the
// isSpilloverEligible unit tests); this proves the sqlite store
// returns the right sentinel when a group's aggregate cap trips, so
// the upstream loop has an accurate error to switch on.
//
// Two groups (grp_full, grp_open). grp_full has its aggregate cap
// already saturated. A pick against grp_full must return
// ErrGroupConcurrencyMax; a pick against grp_open with the same
// params must succeed. The upstream Service loops through the
// candidates in order and reads the error type, so this pair of
// picks is the minimum guarantee the pool layer must provide.

import (
	"context"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func TestPickAndLock_SpilloverContract(t *testing.T) {
	db := openMem(t)
	accStore := NewAccountStore(db)
	grpStore := NewGroupStore(db)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Two groups. grp_full has one member already loaded to
	// cap=1; grp_open has an idle member. The Service-layer loop
	// (proxy.Service.Generate) will try grp_full first, receive
	// ErrGroupConcurrencyMax, and fall over to grp_open.
	must(grpStore.Create(ctx, sampleGroup("grp_full", "full")))
	must(grpStore.Create(ctx, sampleGroup("grp_open", "open")))

	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_full", Email: "full@example.com",
		Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status: domain.StatusActive, InFlightJobs: 1,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(accStore.Upsert(ctx, &domain.Account{
		ID: "acc_open", Email: "open@example.com",
		Password: "-", SessionID: "-", CookiesJSON: "{}", UserAgent: "-",
		PlanType: domain.PlanPlus, SubscriptionBalance: 100000,
		Status:       domain.StatusActive,
		RegisteredAt: time.Now(), ImportedAt: time.Now(),
	}))
	must(grpStore.AddMember(ctx, "grp_full", "acc_full", 100))
	must(grpStore.AddMember(ctx, "grp_open", "acc_open", 100))

	// Attempt 1: grp_full is at cap=1 with one in-flight → must return
	// ErrGroupConcurrencyMax.
	if _, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		GroupID:           "grp_full",
		MaxGroupInFlight:  1,
	}); err != domain.ErrGroupConcurrencyMax {
		t.Fatalf("saturated group pick: got %v want ErrGroupConcurrencyMax", err)
	}

	// Attempt 2: same params against grp_open → must succeed.
	// This is the exact transition Service.Generate performs after
	// catching the first attempt's error.
	got, _, err := accStore.PickAndLock(ctx, ports.PickParams{
		EstCostHundredths: 100,
		GroupID:           "grp_open",
		MaxGroupInFlight:  1,
	})
	if err != nil {
		t.Fatalf("spillover pick: got err=%v want success", err)
	}
	if got.ID != "acc_open" {
		t.Errorf("spillover pick account: got %s want acc_open", got.ID)
	}
}
