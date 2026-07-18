package jsonstatic

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func testPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs("../../../../data/reference/verified-models.json")
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestLoadsRealVerifiedModels(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	models := r.List(struct {
		Output            string
		IncludeUnstable   bool
		IncludeDeprecated bool
	}{IncludeUnstable: true})
	if len(models) < 100 {
		t.Errorf("expected >=100 models, got %d", len(models))
	}

	// Known models we've verified in earlier sessions.
	for _, alias := range []string{"seedance-2-0-mini", "nano-banana-2", "veo3-1", "text2speech"} {
		spec, err := r.Resolve(alias)
		if err != nil {
			t.Errorf("resolve %q: %v", alias, err)
			continue
		}
		if spec.Alias != alias {
			t.Errorf("alias mismatch %q: got %q", alias, spec.Alias)
		}
		if spec.JST == "" || spec.Endpoint == "" {
			t.Errorf("model %q missing jst or endpoint: %+v", alias, spec)
		}
	}

	// *_unlimited aliases must resolve transparently to the base model.
	spec, err := r.Resolve("seedance-mini-unlimited")
	if err != nil {
		t.Fatalf("resolve alias: %v", err)
	}
	if spec.AliasStrategy != domain.AliasTransparent {
		t.Errorf("expected transparent alias, got %q", spec.AliasStrategy)
	}
	if spec.AliasOf != "seedance-2-0-mini" {
		t.Errorf("alias base: got %q want seedance-2-0-mini", spec.AliasOf)
	}
	if spec.JST != "seedance_2_0_mini" {
		t.Errorf("alias target jst: got %q", spec.JST)
	}
}

func TestResolveUnknownReturnsNotFound(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = r.Resolve("no-such-model")
	if !errors.Is(err, domain.ErrModelNotFound) {
		t.Fatalf("expected ErrModelNotFound, got %v", err)
	}
}

// TestTierFieldsPopulate exercises the round-2 tier attribution path:
// each known SEALED gate maps its JSTs to a specific (starter_locked,
// requires_paid, requires_ultra, tier_source, min_credits_hundredths)
// tuple. We spot-check one alias per bucket so a regression in the
// gen-verified-mapping.mjs → dump-verified-models.mjs chain is loud.
func TestTierFieldsPopulate(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// A1 · Pro gate (SEALED d_class_by_gate:veo3_family_pro).
	// veo3-1 → starter_locked=T, requires_paid=T, tier_source="sealed:veo3_family_pro".
	spec, err := r.Resolve("veo3-1")
	if err != nil {
		t.Fatalf("resolve veo3-1: %v", err)
	}
	if !spec.StarterLocked || !spec.RequiresPaid || spec.RequiresUltra {
		t.Errorf("veo3-1 pro flags: locked=%v paid=%v ultra=%v", spec.StarterLocked, spec.RequiresPaid, spec.RequiresUltra)
	}
	if spec.TierSource != "sealed:veo3_family_pro" {
		t.Errorf("veo3-1 tier_source: got %q", spec.TierSource)
	}

	// A1' · Credit gate (SEALED d_class_by_gate:credits_shortfall).
	// flux-kontext → tier_source starts with "credits_" AND min_credits_hundredths > 0.
	spec, err = r.Resolve("flux-kontext")
	if err != nil {
		t.Fatalf("resolve flux-kontext: %v", err)
	}
	if !spec.StarterLocked || !spec.RequiresPaid {
		t.Errorf("flux-kontext credit flags: locked=%v paid=%v", spec.StarterLocked, spec.RequiresPaid)
	}
	if spec.TierSource != "credits_shortfall" {
		t.Errorf("flux-kontext tier_source: got %q", spec.TierSource)
	}
	if spec.MinCreditsHundredths <= 0 {
		t.Errorf("flux-kontext expected non-zero min_credits_hundredths, got %d", spec.MinCreditsHundredths)
	}

	// A2 · Ultra alias (SEALED d_class_by_gate:unlimited_subscription).
	// seedance-mini-unlimited → transparent alias, requires_ultra=T,
	// tier_source="sealed:unlimited_subscription" — the base spec's own
	// tier must be overridden here.
	spec, err = r.Resolve("seedance-mini-unlimited")
	if err != nil {
		t.Fatalf("resolve seedance-mini-unlimited: %v", err)
	}
	if !spec.StarterLocked || !spec.RequiresPaid || !spec.RequiresUltra {
		t.Errorf("ultra flags: locked=%v paid=%v ultra=%v", spec.StarterLocked, spec.RequiresPaid, spec.RequiresUltra)
	}
	if spec.TierSource != "sealed:unlimited_subscription" {
		t.Errorf("ultra tier_source: got %q", spec.TierSource)
	}

	// A3 · image_models.mjs starter-safe passthrough. z-image → all
	// flags false, tier_source="mapping:image_models.mjs".
	spec, err = r.Resolve("z-image")
	if err != nil {
		t.Fatalf("resolve z-image: %v", err)
	}
	if spec.StarterLocked || spec.RequiresPaid || spec.RequiresUltra {
		t.Errorf("z-image expected all-false tier flags: %+v", spec)
	}
	if spec.TierSource != "mapping:image_models.mjs" {
		t.Errorf("z-image tier_source: got %q", spec.TierSource)
	}

	// A4 · No evidence → assumed_starter_safe. Pick one B-class model
	// we know isn't in any SEALED gate.
	spec, err = r.Resolve("autosprite")
	if err != nil {
		t.Fatalf("resolve autosprite: %v", err)
	}
	if spec.StarterLocked || spec.RequiresPaid {
		t.Errorf("autosprite expected starter-safe: locked=%v paid=%v", spec.StarterLocked, spec.RequiresPaid)
	}
	if spec.TierSource != "assumed_starter_safe" {
		t.Errorf("autosprite tier_source: got %q", spec.TierSource)
	}
}

// TestStarterLockedSyncsFromJSON asserts the per-JST locked set is
// populated from the starter_locked flag in the JSON dump — no
// separate StarterLockedPath needed for round 2.
func TestStarterLockedSyncsFromJSON(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// veo3_1 is in SEALED:veo3_family_pro — must be starter-locked.
	if !r.StarterLocked("veo3_1") {
		t.Error("expected veo3_1 to be starter-locked")
	}
	// z_image is not gated anywhere — must NOT be starter-locked.
	if r.StarterLocked("z_image") {
		t.Error("expected z_image to NOT be starter-locked")
	}
}
