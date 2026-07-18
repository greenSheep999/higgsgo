package jsonstatic

// Registry override merging tests: verifies that a wired-in
// ports.ModelOverrideStore is layered on top of the static catalog
// after Reload, and that Resolve() / List() emit the merged view.

import (
	"context"
	"reflect"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// stubOverrideStore is a minimal ports.ModelOverrideStore used by the
// registry merge tests. It never fails and only supports List — the
// merge path is the only surface the registry consumes.
type stubOverrideStore struct {
	rows []domain.ModelOverride
}

func (s *stubOverrideStore) Get(context.Context, string) (*domain.ModelOverride, error) {
	return nil, nil
}
func (s *stubOverrideStore) Upsert(context.Context, *domain.ModelOverride) error { return nil }
func (s *stubOverrideStore) Delete(context.Context, string) error                { return nil }
func (s *stubOverrideStore) List(context.Context) ([]domain.ModelOverride, error) {
	return s.rows, nil
}

var _ ports.ModelOverrideStore = (*stubOverrideStore)(nil)

// TestRegistry_OverrideMergesOnResolve exercises the tier + credit
// pointer semantics: nil = inherit spec, set = replace.
func TestRegistry_OverrideMergesOnResolve(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Baseline: nano-banana-2 without any override.
	base, err := r.Resolve("nano-banana-2")
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}

	// Layer an override: force RequiresUltra=true, expand alias set.
	ultra := true
	minCr := int64(base.MinCreditsHundredths + 10_000)
	store := &stubOverrideStore{
		rows: []domain.ModelOverride{
			{
				Alias:                "nano-banana-2",
				RequiresUltra:        &ultra,
				MinCreditsHundredths: &minCr,
				ExtraAliases:         []string{"banana-2-plus", "google-nano-banana-2"},
				Note:                 "downstream new-api dual-registration",
			},
		},
	}
	r.SetOverrideProvider(store)
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("reload with overrides: %v", err)
	}

	got, err := r.Resolve("nano-banana-2")
	if err != nil {
		t.Fatalf("resolve after override: %v", err)
	}
	if !got.RequiresUltra {
		t.Errorf("RequiresUltra should be true after override")
	}
	if got.MinCreditsHundredths != minCr {
		t.Errorf("min_credits_hundredths: got %d, want %d", got.MinCreditsHundredths, minCr)
	}
	if !reflect.DeepEqual(got.ExtraAliases, []string{"banana-2-plus", "google-nano-banana-2"}) {
		t.Errorf("extra_aliases: got %v", got.ExtraAliases)
	}
	if got.Note != "downstream new-api dual-registration" {
		t.Errorf("note: got %q", got.Note)
	}

	// The base spec pointer in the map must NOT be mutated in place.
	// A second Resolve should be identical (deep) — regression on
	// aliasing.
	got2, _ := r.Resolve("nano-banana-2")
	if got2.MinCreditsHundredths != minCr {
		t.Errorf("second resolve dropped override: got %d", got2.MinCreditsHundredths)
	}
}

// TestRegistry_OverrideList_HandsBackCopies verifies List returns
// override-merged copies and does not leak into the shared map. This
// covers the pointer-vs-copy trap that would otherwise mutate the
// canonical spec across concurrent readers.
func TestRegistry_OverrideList_HandsBackCopies(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	paid := true
	r.SetOverrideProvider(&stubOverrideStore{
		rows: []domain.ModelOverride{{Alias: "nano-banana-2", RequiresPaid: &paid, Note: "hint"}},
	})
	if err := r.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	list := r.List(ports.ModelFilter{IncludeUnstable: true})
	var seen *domain.ModelSpec
	for _, s := range list {
		if s.Alias == "nano-banana-2" {
			seen = s
			break
		}
	}
	if seen == nil {
		t.Fatal("nano-banana-2 missing from List")
	}
	if !seen.RequiresPaid {
		t.Error("List entry should have RequiresPaid=true after override")
	}
	if seen.Note != "hint" {
		t.Errorf("List entry note: got %q", seen.Note)
	}

	// Mutate the returned copy — a second List call must not observe it.
	seen.RequiresPaid = false
	seen.Note = ""
	list2 := r.List(ports.ModelFilter{IncludeUnstable: true})
	for _, s := range list2 {
		if s.Alias == "nano-banana-2" {
			if !s.RequiresPaid {
				t.Error("List leaked shared state: second call missing override")
			}
			if s.Note != "hint" {
				t.Errorf("List leaked shared state on note field: got %q", s.Note)
			}
		}
	}
}

// TestRegistry_NoOverrideProvider_IsUnchanged ensures the registry
// keeps its pre-015 behaviour when no override provider is wired.
func TestRegistry_NoOverrideProvider_IsUnchanged(t *testing.T) {
	r, err := New(Config{Path: testPath(t)})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	spec, err := r.Resolve("nano-banana-2")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(spec.ExtraAliases) != 0 {
		t.Errorf("without provider, extra_aliases should be empty: %v", spec.ExtraAliases)
	}
	if spec.Note != "" {
		t.Errorf("without provider, note should be empty: %q", spec.Note)
	}
}
