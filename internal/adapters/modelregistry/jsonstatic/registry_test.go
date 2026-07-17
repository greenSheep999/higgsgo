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
