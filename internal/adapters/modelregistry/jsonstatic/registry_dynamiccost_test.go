package jsonstatic

// Tests for SetDynamicCosts: the live cost overlay pushed by the costsync
// ticker must override each spec's static EstCostHundredths at Resolve()
// and List() time, keyed by JST, and survive a Reload().

import (
	"context"
	"testing"

	"github.com/greensheep999/higgsgo/internal/ports"
)

func TestSetDynamicCosts_OverridesResolve(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// Baseline: read the static cost + JST for a known video model.
	const alias = "seedance-2-0-mini"
	base, err := r.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve %q: %v", alias, err)
	}
	jst := base.JST
	if jst == "" {
		t.Fatalf("alias %q has empty JST", alias)
	}
	const newCost int64 = 4242
	if base.EstCostHundredths == newCost {
		t.Fatalf("test precondition broken: static cost already %d", newCost)
	}

	// Push a live overlay for this JST.
	r.SetDynamicCosts(map[string]int64{jst: newCost})

	got, err := r.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve after overlay: %v", err)
	}
	if got.EstCostHundredths != newCost {
		t.Errorf("Resolve cost = %d, want overlaid %d", got.EstCostHundredths, newCost)
	}

	// A transparent alias onto the same base JST must see the overlay too.
	if alt, err := r.Resolve("seedance-mini-unlimited"); err == nil {
		if alt.JST == jst && alt.EstCostHundredths != newCost {
			t.Errorf("transparent alias cost = %d, want overlaid %d", alt.EstCostHundredths, newCost)
		}
	}

	// List() must reflect the overlay as well.
	for _, spec := range r.List(ports.ModelFilter{IncludeUnstable: true, IncludeDeprecated: true}) {
		if spec.Alias == alias && spec.EstCostHundredths != newCost {
			t.Errorf("List cost for %q = %d, want overlaid %d", alias, spec.EstCostHundredths, newCost)
		}
	}

	// Clearing the overlay reverts to the static cost.
	r.SetDynamicCosts(nil)
	reverted, err := r.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve after clear: %v", err)
	}
	if reverted.EstCostHundredths != base.EstCostHundredths {
		t.Errorf("after clear cost = %d, want static %d", reverted.EstCostHundredths, base.EstCostHundredths)
	}
}

func TestSetDynamicCosts_SurvivesReload(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	const alias = "seedance-2-0-mini"
	base, err := r.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	const newCost int64 = 9999
	r.SetDynamicCosts(map[string]int64{base.JST: newCost})

	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	got, err := r.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve after reload: %v", err)
	}
	if got.EstCostHundredths != newCost {
		t.Errorf("overlay dropped by Reload: cost = %d, want %d", got.EstCostHundredths, newCost)
	}
}

// A zero / negative overlay entry must be ignored (defensive: a bad
// upstream reduction should never zero out a real cost).
func TestSetDynamicCosts_IgnoresNonPositive(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	const alias = "seedance-2-0-mini"
	base, err := r.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r.SetDynamicCosts(map[string]int64{base.JST: 0})
	got, err := r.Resolve(alias)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.EstCostHundredths != base.EstCostHundredths {
		t.Errorf("zero overlay applied: cost = %d, want static %d", got.EstCostHundredths, base.EstCostHundredths)
	}
}
