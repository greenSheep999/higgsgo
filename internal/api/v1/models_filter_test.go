package v1

// Tests for GET /v1/models (HandleModelsList):
//
//   - default: no filter returns every non-unstable/non-deprecated model
//   - output filter narrows to a single Output family
//   - requires_paid filter obeys the tri-state flag
//   - q= substring match is case-insensitive over the alias
//   - limit/offset paginate without duplicates and cap at maxModelsListLimit
//   - invalid limit / invalid bool query params render a 400

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeModelRegistry is a minimal ports.ModelRegistry stub. It stores a
// hand-crafted slice and applies exactly the same output / include_*
// filter semantics the jsonstatic registry does, so the handler-side
// filter logic is what's really under test.
type fakeModelRegistry struct {
	specs []*domain.ModelSpec
}

func (f *fakeModelRegistry) Resolve(alias string) (*domain.ModelSpec, error) {
	for _, s := range f.specs {
		if s.Alias == alias {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", domain.ErrModelNotFound, alias)
}

func (f *fakeModelRegistry) List(filter ports.ModelFilter) []*domain.ModelSpec {
	out := make([]*domain.ModelSpec, 0, len(f.specs))
	for _, s := range f.specs {
		if filter.Output != "" && s.Output != filter.Output {
			continue
		}
		if !filter.IncludeUnstable && s.Unstable {
			continue
		}
		if !filter.IncludeDeprecated && s.Deprecated {
			continue
		}
		out = append(out, s)
	}
	return out
}

func (f *fakeModelRegistry) Reload(context.Context) error       { return nil }
func (f *fakeModelRegistry) ResolveAlias(string) (string, bool) { return "", false }
func (f *fakeModelRegistry) StarterLocked(string) bool          { return false }

// sampleSpecs returns a small, hand-crafted registry that spans every
// axis the handler filters on: two Outputs, both requires_paid states,
// both requires_unlim states, and alias substrings for the q= test.
// Order is deterministic so pagination assertions can name IDs directly.
func sampleSpecs() []*domain.ModelSpec {
	return []*domain.ModelSpec{
		{Alias: "seedance-2-0", JST: "seedance_2_0", Output: "video", RequiresPaid: false, RequiresUnlim: false},
		{Alias: "seedance-2-0-mini", JST: "seedance_2_0_mini", Output: "video", RequiresPaid: true, RequiresUnlim: false},
		{Alias: "veo3-1", JST: "veo3_1", Output: "video", RequiresPaid: true, RequiresUnlim: true},
		{Alias: "nano-banana-2", JST: "nano_banana_2", Output: "image", RequiresPaid: false, RequiresUnlim: false},
		{Alias: "flux-pro", JST: "flux_pro", Output: "image", RequiresPaid: true, RequiresUnlim: false},
		{Alias: "kling-lipsync", JST: "kling_lipsync", Output: "audio", RequiresPaid: false, RequiresUnlim: false},
	}
}

func newModelsListRouter(t *testing.T, reg ports.ModelRegistry) chi.Router {
	t.Helper()
	h := &Handler{Registry: reg}
	r := chi.NewRouter()
	r.Get("/v1/models", h.HandleModelsList)
	return r
}

// decodeModelsBody unmarshals the response into a typed shape so tests
// aren't repeating map[string]any casts.
type modelsBody struct {
	Object            string           `json:"object"`
	Data              []map[string]any `json:"data"`
	Limit             int              `json:"limit"`
	Offset            int              `json:"offset"`
	TotalBeforePaging int              `json:"total_before_pagination"`
}

func doGet(t *testing.T, r chi.Router, target string) (*httptest.ResponseRecorder, modelsBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var body modelsBody
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v; raw=%s", target, err, rec.Body.String())
		}
	}
	return rec, body
}

func TestModelsList_NoFilter(t *testing.T) {
	reg := &fakeModelRegistry{specs: sampleSpecs()}
	r := newModelsListRouter(t, reg)

	rec, body := doGet(t, r, "/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := body.Object, "list"; got != want {
		t.Errorf("object: got %q want %q", got, want)
	}
	if got, want := len(body.Data), len(sampleSpecs()); got != want {
		t.Errorf("data len: got %d want %d", got, want)
	}
	if got, want := body.TotalBeforePaging, len(sampleSpecs()); got != want {
		t.Errorf("total_before_pagination: got %d want %d", got, want)
	}
	if got, want := body.Limit, defaultModelsListLimit; got != want {
		t.Errorf("limit: got %d want %d", got, want)
	}
	if got, want := body.Offset, 0; got != want {
		t.Errorf("offset: got %d want %d", got, want)
	}
}

func TestModelsList_OutputFilter(t *testing.T) {
	reg := &fakeModelRegistry{specs: sampleSpecs()}
	r := newModelsListRouter(t, reg)

	rec, body := doGet(t, r, "/v1/models?output=image")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 2; got != want {
		t.Fatalf("image count: got %d want %d; body=%v", got, want, body.Data)
	}
	for _, m := range body.Data {
		if m["output"] != "image" {
			t.Errorf("row output: got %v want image; row=%v", m["output"], m)
		}
	}
	if body.TotalBeforePaging != 2 {
		t.Errorf("total_before_pagination: got %d want 2", body.TotalBeforePaging)
	}
}

func TestModelsList_RequiresPaidFilter(t *testing.T) {
	reg := &fakeModelRegistry{specs: sampleSpecs()}
	r := newModelsListRouter(t, reg)

	// requires_paid=true selects exactly the three paid rows.
	rec, body := doGet(t, r, "/v1/models?requires_paid=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 3; got != want {
		t.Fatalf("paid count: got %d want %d; body=%v", got, want, body.Data)
	}
	for _, m := range body.Data {
		if m["requires_paid"] != true {
			t.Errorf("row requires_paid: got %v want true; row=%v", m["requires_paid"], m)
		}
	}

	// requires_paid=false selects the free rows.
	rec, body = doGet(t, r, "/v1/models?requires_paid=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 3; got != want {
		t.Fatalf("free count: got %d want %d; body=%v", got, want, body.Data)
	}
	for _, m := range body.Data {
		if m["requires_paid"] != false {
			t.Errorf("row requires_paid: got %v want false; row=%v", m["requires_paid"], m)
		}
	}
}

func TestModelsList_RequiresUnlimFilter(t *testing.T) {
	reg := &fakeModelRegistry{specs: sampleSpecs()}
	r := newModelsListRouter(t, reg)

	rec, body := doGet(t, r, "/v1/models?requires_unlim=true")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 1; got != want {
		t.Fatalf("unlim count: got %d want %d; body=%v", got, want, body.Data)
	}
	if id := body.Data[0]["id"]; id != "veo3-1" {
		t.Errorf("unlim row id: got %v want veo3-1", id)
	}
}

func TestModelsList_QFilter(t *testing.T) {
	reg := &fakeModelRegistry{specs: sampleSpecs()}
	r := newModelsListRouter(t, reg)

	// Case-insensitive substring: capital "SEEDANCE" must still match.
	rec, body := doGet(t, r, "/v1/models?q=SEEDANCE")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 2; got != want {
		t.Fatalf("seedance count: got %d want %d; body=%v", got, want, body.Data)
	}
	for _, m := range body.Data {
		id, _ := m["id"].(string)
		if id != "seedance-2-0" && id != "seedance-2-0-mini" {
			t.Errorf("unexpected q hit: %v", id)
		}
	}

	// No match => empty data but still a 200 with the total set to 0.
	rec, body = doGet(t, r, "/v1/models?q=doesnotexist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(body.Data) != 0 {
		t.Errorf("expected empty data on q miss, got %v", body.Data)
	}
	if body.TotalBeforePaging != 0 {
		t.Errorf("expected total 0 on q miss, got %d", body.TotalBeforePaging)
	}
}

func TestModelsList_Pagination(t *testing.T) {
	// Bloat the registry so we can page multiple times without hitting
	// filter side-effects.
	specs := make([]*domain.ModelSpec, 0, 12)
	for i := 0; i < 12; i++ {
		specs = append(specs, &domain.ModelSpec{
			Alias:  fmt.Sprintf("m-%02d", i),
			JST:    fmt.Sprintf("m_%02d", i),
			Output: "image",
		})
	}
	reg := &fakeModelRegistry{specs: specs}
	r := newModelsListRouter(t, reg)

	// First page: 5 rows.
	rec, body := doGet(t, r, "/v1/models?limit=5&offset=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 5; got != want {
		t.Fatalf("first page len: got %d want %d", got, want)
	}
	if got, want := body.TotalBeforePaging, 12; got != want {
		t.Errorf("total_before_pagination: got %d want %d", got, want)
	}
	firstIDs := make(map[string]struct{}, 5)
	for _, m := range body.Data {
		firstIDs[m["id"].(string)] = struct{}{}
	}

	// Second page: another 5 rows, all distinct from the first page.
	rec, body = doGet(t, r, "/v1/models?limit=5&offset=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 5; got != want {
		t.Fatalf("second page len: got %d want %d", got, want)
	}
	for _, m := range body.Data {
		id := m["id"].(string)
		if _, dup := firstIDs[id]; dup {
			t.Errorf("second page repeats id from first page: %s", id)
		}
	}

	// Tail page: only 2 rows left.
	rec, body = doGet(t, r, "/v1/models?limit=5&offset=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := len(body.Data), 2; got != want {
		t.Fatalf("tail page len: got %d want %d", got, want)
	}

	// Offset past the end: empty page, no error.
	rec, body = doGet(t, r, "/v1/models?limit=5&offset=999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(body.Data) != 0 {
		t.Errorf("offset past end should be empty, got %v", body.Data)
	}
	if body.TotalBeforePaging != 12 {
		t.Errorf("total still reflects filter result: got %d want 12", body.TotalBeforePaging)
	}
}

func TestModelsList_LimitCap(t *testing.T) {
	// Enough specs to exceed the cap so we can verify the response echoes
	// the capped limit AND caps the returned rows.
	specs := make([]*domain.ModelSpec, 0, maxModelsListLimit+50)
	for i := 0; i < maxModelsListLimit+50; i++ {
		specs = append(specs, &domain.ModelSpec{
			Alias:  fmt.Sprintf("cap-%04d", i),
			Output: "image",
		})
	}
	reg := &fakeModelRegistry{specs: specs}
	r := newModelsListRouter(t, reg)

	rec, body := doGet(t, r, "/v1/models?limit=99999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := body.Limit, maxModelsListLimit; got != want {
		t.Errorf("echoed limit not capped: got %d want %d", got, want)
	}
	if got, want := len(body.Data), maxModelsListLimit; got != want {
		t.Errorf("data len not capped: got %d want %d", got, want)
	}
}

func TestModelsList_InvalidLimit(t *testing.T) {
	reg := &fakeModelRegistry{specs: sampleSpecs()}
	r := newModelsListRouter(t, reg)

	rec, _ := doGet(t, r, "/v1/models?limit=abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body.error missing or wrong type: %T", body["error"])
	}
	if errObj["type"] != "invalid_query" {
		t.Errorf("error.type: got %v want invalid_query", errObj["type"])
	}
}

func TestModelsList_InvalidBoolFilter(t *testing.T) {
	reg := &fakeModelRegistry{specs: sampleSpecs()}
	r := newModelsListRouter(t, reg)

	rec, _ := doGet(t, r, "/v1/models?requires_paid=maybe")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
}
