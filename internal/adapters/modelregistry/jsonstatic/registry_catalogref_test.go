package jsonstatic

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestLoadBodyTemplates_SkipsAppleDoubleAndHidden asserts that the
// loader ignores macOS metadata siblings (`._foo.json`) and any other
// dotfile that survives a naive `tar cf` on an HFS+/APFS source. The
// AppleDouble files are binary and would fail json.Unmarshal — the
// loader must treat them as invisible so a mis-configured deploy
// pipeline doesn't crash the boot with an opaque parse error.
func TestLoadBodyTemplates_SkipsAppleDoubleAndHidden(t *testing.T) {
	dir := t.TempDir()

	real := `{"alias":"real-model","exampleBody":{"prompt":"hi"}}`
	if err := os.WriteFile(filepath.Join(dir, "real.json"), []byte(real), 0o644); err != nil {
		t.Fatal(err)
	}
	// Binary garbage that would blow up json.Unmarshal if the loader
	// let it through.
	garbage := []byte{0x00, 0x05, 0x16, 0x07, 0x00, 0x02, 0x00}
	if err := os.WriteFile(filepath.Join(dir, "._real.json"), garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden.json"), garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadBodyTemplates(dir)
	if err != nil {
		t.Fatalf("loadBodyTemplates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 template, got %d: %v", len(got), keys(got))
	}
	if _, ok := got["real-model"]; !ok {
		t.Errorf("expected 'real-model' key, got %v", keys(got))
	}
}

// TestNormalizeCatalogRef pins the grammar accepted by the resolver.
// Every shape observed in data/reference/body-templates has a case
// here — adding a new shape requires adding a case, not silently
// papering over an unknown-ref count in the real-data test below.
func TestNormalizeCatalogRef(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		wantKind      refKind
		wantPath      string
		wantExtractor string
		wantLiteral   []string
	}{
		{
			name:        "literal enum simple",
			raw:         "literal enum: insert_after|replace",
			wantKind:    refLiteral,
			wantLiteral: []string{"insert_after", "replace"},
		},
		{
			name:        "literal enum with quoted value and trailing annotation",
			raw:         `literal enum: "preset" (also used with voice_id from reference_voices)`,
			wantKind:    refLiteral,
			wantLiteral: []string{"preset"},
		},
		{
			name:        "literal single quoted",
			raw:         `literal "skin-enhancer"`,
			wantKind:    refLiteral,
			wantLiteral: []string{"skin-enhancer"},
		},
		{
			name:        "literal single with annotation",
			raw:         `literal "skin-enhancer" (body top-level)`,
			wantKind:    refLiteral,
			wantLiteral: []string{"skin-enhancer"},
		},
		{
			name:          "catalog with unicode arrow extractor",
			raw:           "catalogs/camera_settings.json → item.camera.id",
			wantKind:      refCatalog,
			wantPath:      "catalogs/camera_settings.json",
			wantExtractor: "item.camera.id",
		},
		{
			name:          "catalog with ascii arrow extractor",
			raw:           "catalogs/camera_settings.json -> item.camera.id",
			wantKind:      refCatalog,
			wantPath:      "catalogs/camera_settings.json",
			wantExtractor: "item.camera.id",
		},
		{
			name:     "catalog with trailing annotation",
			raw:      "catalogs/reference_elements.json (must be user-created; empty by default)",
			wantKind: refCatalog,
			wantPath: "catalogs/reference_elements.json",
		},
		{
			name:     "catalog bare",
			raw:      "catalogs/motions.json",
			wantKind: refCatalog,
			wantPath: "catalogs/motions.json",
		},
		{
			name:     "spa-only unknown",
			raw:      "spa-only-presets.json → cinema_studio_3_5_video_edit",
			wantKind: refUnknown,
		},
		{
			name:     "server-data unknown",
			raw:      "server/data/spa-only-presets.json → nano_banana_2_skin_enhancer.candidates[]",
			wantKind: refUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, e, l, k := normalizeCatalogRef(tc.raw)
			if k != tc.wantKind {
				t.Errorf("kind: got %v want %v", k, tc.wantKind)
			}
			if p != tc.wantPath {
				t.Errorf("path: got %q want %q", p, tc.wantPath)
			}
			if e != tc.wantExtractor {
				t.Errorf("extractor: got %q want %q", e, tc.wantExtractor)
			}
			if !reflect.DeepEqual(l, tc.wantLiteral) {
				t.Errorf("literal: got %v want %v", l, tc.wantLiteral)
			}
		})
	}
}

// TestExtractFromCatalogTree drives the walker against the real
// camera_settings.json fixture. `item.camera.id` must yield the six
// UUIDs (with the intentional duplicate — the walker doesn't dedupe;
// downstream consumers do if they need distinct values).
func TestExtractFromCatalogTree(t *testing.T) {
	abs, err := filepath.Abs("../../../../data/reference/catalogs/camera_settings.json")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	ids := extractFromCatalogTree(body, "item.camera.id")
	if len(ids) != 6 {
		t.Fatalf("expected 6 camera ids, got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if len(id) != 36 || strings.Count(id, "-") != 4 {
			t.Errorf("not a uuid-shaped string: %q", id)
		}
	}
}

// knownUnresolvedRefs is the allowlist of catalogRef strings that we
// know cannot be resolved from on-disk catalogs. Three shapes end up
// here:
//
//   - spa-only-presets.json refs — the SPA generates these client-side
//     from ephemeral job state; there is no on-disk catalog to load.
//   - server/data/... refs — annotations pointing at server-owned
//     data structures we don't ship in this repo.
//   - catalogs/reference_elements.json — a legitimate catalog file
//     that is intentionally empty by default (must be user-created;
//     see the annotation in the body-template files). It classifies
//     as refCatalog but resolves to zero ids, so we allowlist it
//     rather than pretend the file is missing.
var knownUnresolvedRefs = map[string]bool{
	"spa-only-presets.json → nano_banana_animal":                                                                  true,
	"spa-only-presets.json → cinema_studio_3_5_video_edit (SPA-generated artifact uuid tied to that parent)":      true,
	"spa-only-presets.json → cinema_studio_3_5_video_edit (must be completed cinematic_studio_video_3_5 job in same account)": true,
	"server/data/spa-only-presets.json → nano_banana_2_skin_enhancer.candidates[]":                                true,
	"catalogs/reference_elements.json (must be user-created; empty by default)":                                   true,
	// marketing_brand_kits / marketing_products are on-disk catalogs
	// that ship empty — the operator populates them via a scraper /
	// user upload before use. Same "empty by default" shape as
	// reference_elements above.
	"catalogs/marketing_brand_kits.json (must be user-scraped first)": true,
	"catalogs/marketing_products.json (user-created)":                 true,
}

// TestCatalogRefs_RealDataResolves iterates every catalogRef in the
// shipped body-templates and asserts each one either (a) normalises to
// a catalog that resolves — flat or via extractor — with at least one
// value, (b) normalises to a literal with at least one value, or (c)
// appears on the allowlist above. A new unresolved ref must earn its
// place on the allowlist rather than silently dropping enum values.
func TestCatalogRefs_RealDataResolves(t *testing.T) {
	tplDir, err := filepath.Abs("../../../../data/reference/body-templates")
	if err != nil {
		t.Fatal(err)
	}
	catDir, err := filepath.Abs("../../../../data/reference/catalogs")
	if err != nil {
		t.Fatal(err)
	}

	catalogs, trees, err := loadCatalogs(catDir)
	if err != nil {
		t.Fatalf("loadCatalogs: %v", err)
	}

	entries, err := os.ReadDir(tplDir)
	if err != nil {
		t.Fatalf("read body-templates dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(tplDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var raw struct {
			CatalogRefs map[string]string `json:"catalogRefs"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for param, ref := range raw.CatalogRefs {
			checked++
			if knownUnresolvedRefs[ref] {
				continue
			}
			path, extractor, literal, kind := normalizeCatalogRef(ref)
			switch kind {
			case refLiteral:
				if len(literal) == 0 {
					t.Errorf("%s: param %q literal %q produced no values",
						e.Name(), param, ref)
				}
			case refCatalog:
				resolved := false
				if extractor != "" {
					if tree, ok := trees[path]; ok {
						if vals := extractFromCatalogTree(tree, extractor); len(vals) > 0 {
							resolved = true
						}
					}
				}
				if !resolved {
					if ids, ok := catalogs[path]; ok && len(ids) > 0 {
						resolved = true
					}
				}
				if !resolved {
					t.Errorf("%s: param %q ref %q did not resolve (path=%q extractor=%q)",
						e.Name(), param, ref, path, extractor)
				}
			case refUnknown:
				t.Errorf("%s: param %q ref %q normalised to refUnknown and is not allowlisted",
					e.Name(), param, ref)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no catalogRefs found in body-templates — fixture missing?")
	}
	t.Logf("checked %d catalogRef entries across %d templates", checked, len(entries))
}

// keys returns the sorted key list of a bodyTemplate map for readable
// failure messages.
func keys(m map[string]bodyTemplate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// rawKeys returns the sorted key list of a json.RawMessage map.
func rawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestLoadCatalogs_SkipsAppleDoubleAndHidden mirrors the body-templates
// check for the catalogs directory. Same failure mode, same fix.
func TestLoadCatalogs_SkipsAppleDoubleAndHidden(t *testing.T) {
	dir := t.TempDir()

	real := `{"items":[{"id":"aaa"},{"id":"bbb"}]}`
	if err := os.WriteFile(filepath.Join(dir, "real.json"), []byte(real), 0o644); err != nil {
		t.Fatal(err)
	}
	garbage := []byte{0x00, 0x05, 0x16, 0x07, 0x00, 0x02, 0x00}
	if err := os.WriteFile(filepath.Join(dir, "._real.json"), garbage, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden.json"), garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	ids, trees, err := loadCatalogs(dir)
	if err != nil {
		t.Fatalf("loadCatalogs: %v", err)
	}
	if len(trees) != 1 {
		t.Fatalf("expected exactly 1 catalog tree, got %d: %v", len(trees), rawKeys(trees))
	}
	if _, ok := trees["catalogs/real.json"]; !ok {
		t.Errorf("expected 'catalogs/real.json' tree key, got %v", rawKeys(trees))
	}
	want := []string{"aaa", "bbb"}
	if got := ids["catalogs/real.json"]; !reflect.DeepEqual(got, want) {
		t.Errorf("real.json ids: got %v want %v", got, want)
	}
}
