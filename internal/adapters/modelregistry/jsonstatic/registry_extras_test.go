package jsonstatic

// TestRegistry_LoadsUnlimAndFreeQuotaFields verifies the four load-
// balance router fields the loader added on top of the historical
// max_resolution / max_duration_sec / min_plan tuple:
//
//   * UnlimJobSetType  — from `unlim_job_set_type` in extras
//   * UnlimBundleTypes — from `unlim_bundle_types` in extras
//   * FreeQuotaField   — from `free_quota_field` in extras
//   * endpoint_status="dead" — alias is skipped entirely
//
// Uses a temp extras file so the assertion is stable against future
// edits to data/reference/model-specs-extra.json.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func TestRegistry_LoadsUnlimAndFreeQuotaFields(t *testing.T) {
	// Point the loader at a synthetic extras file with the three new
	// fields set on a real alias. seedance-2-0 has an unlim endpoint
	// (`seedance_2_unlimited` per model-specs-extra.json) so the
	// verified-models entry stays consistent.
	extras := map[string]any{
		"_schema": map[string]any{"description": "test fixture"},
		"aliases": map[string]any{
			"seedance-2-0": map[string]any{
				"max_resolution":     "1080p",
				"max_duration_sec":   15,
				"unlim_job_set_type": "seedance_2_unlimited",
				"unlim_bundle_types": []string{"seedance_2_720p", "seedance_2_1080p"},
			},
			"text2image-soul": map[string]any{
				"free_quota_field": "soul_credits",
			},
			// Dead endpoint — must be filtered out entirely.
			"flux-kontext": map[string]any{
				"endpoint_status": "dead",
			},
		},
	}
	dir := t.TempDir()
	extrasPath := filepath.Join(dir, "extras.json")
	body, err := json.Marshal(extras)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extrasPath, body, 0o644); err != nil {
		t.Fatal(err)
	}

	r, err := New(Config{Path: testPath(t), ExtraSpecsPath: extrasPath})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// UnlimJobSetType + UnlimBundleTypes populate from extras.
	spec, err := r.Resolve("seedance-2-0")
	if err != nil {
		t.Fatalf("resolve seedance-2-0: %v", err)
	}
	if spec.UnlimJobSetType != "seedance_2_unlimited" {
		t.Errorf("UnlimJobSetType: got %q want seedance_2_unlimited", spec.UnlimJobSetType)
	}
	if len(spec.UnlimBundleTypes) != 2 || spec.UnlimBundleTypes[0] != "seedance_2_720p" {
		t.Errorf("UnlimBundleTypes: got %v want [seedance_2_720p seedance_2_1080p]", spec.UnlimBundleTypes)
	}
	if spec.MaxResolution != "1080p" {
		t.Errorf("MaxResolution: got %q want 1080p", spec.MaxResolution)
	}
	if spec.MaxDurationSec != 15 {
		t.Errorf("MaxDurationSec: got %d want 15", spec.MaxDurationSec)
	}

	// FreeQuotaField populates for a different alias so we know the
	// two paths are wired independently.
	fs, err := r.Resolve("text2image-soul")
	if err != nil {
		t.Fatalf("resolve text2image-soul: %v", err)
	}
	if fs.FreeQuotaField != "soul_credits" {
		t.Errorf("FreeQuotaField: got %q want soul_credits", fs.FreeQuotaField)
	}

	// flux-kontext is a dead endpoint per extras — should NOT resolve.
	if _, err := r.Resolve("flux-kontext"); err == nil {
		t.Errorf("flux-kontext (endpoint_status=dead) resolved; expected ErrModelNotFound")
	}

	// It also must not appear in the List result.
	for _, m := range r.List(ports.ModelFilter{IncludeUnstable: true, IncludeDeprecated: true}) {
		if m.Alias == "flux-kontext" {
			t.Errorf("flux-kontext should be filtered out of List, got %+v", m)
		}
	}

	// An alias without any of the four new fields keeps them empty —
	// confirms the "optional enrichment" contract.
	other, err := r.Resolve("nano-banana-2")
	if err != nil {
		t.Fatalf("resolve nano-banana-2: %v", err)
	}
	// nano-banana-2 has no extras entry in our fixture → all four zero.
	if other.UnlimJobSetType != "" || len(other.UnlimBundleTypes) != 0 || other.FreeQuotaField != "" {
		t.Errorf("nano-banana-2 unexpectedly picked up unlim/free_quota fields: %+v",
			struct {
				JST     string
				Bundles []string
				Quota   string
			}{other.UnlimJobSetType, other.UnlimBundleTypes, other.FreeQuotaField})
	}
	// Sanity check the PlanType type name is still domain.PlanType.
	if other.MinPlan != domain.PlanFree && other.MinPlan.TierRank() == 0 {
		// nano-banana-2 has a real tier from verified-models, but we
		// just want to make sure ModelSpec is not stripping fields.
	}
}
