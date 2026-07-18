package admin

// Tests for the /admin/models/overrides surface. Fake ports.
// ModelOverrideStore records writes so we can assert that the handler
// hands the correct row shape to the store.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
)

type fakeOverrideStore struct {
	rows map[string]*domain.ModelOverride

	lastUpsert *domain.ModelOverride
	deleted    []string
}

func newFakeOverrideStore() *fakeOverrideStore {
	return &fakeOverrideStore{rows: map[string]*domain.ModelOverride{}}
}

func (f *fakeOverrideStore) Get(_ context.Context, alias string) (*domain.ModelOverride, error) {
	o, ok := f.rows[alias]
	if !ok {
		return nil, nil
	}
	c := *o
	return &c, nil
}

func (f *fakeOverrideStore) Upsert(_ context.Context, o *domain.ModelOverride) error {
	copy := *o
	f.rows[o.Alias] = &copy
	f.lastUpsert = &copy
	return nil
}

func (f *fakeOverrideStore) Delete(_ context.Context, alias string) error {
	delete(f.rows, alias)
	f.deleted = append(f.deleted, alias)
	return nil
}

func (f *fakeOverrideStore) List(context.Context) ([]domain.ModelOverride, error) {
	out := make([]domain.ModelOverride, 0, len(f.rows))
	for _, o := range f.rows {
		out = append(out, *o)
	}
	return out, nil
}

func newOverridesRouter(store *fakeOverrideStore) chi.Router {
	r := chi.NewRouter()
	NewModelOverridesHandler(store, nil /* registry */, nil).Register(r)
	return r
}

func TestModelOverrides_Put_UpsertsRow(t *testing.T) {
	store := newFakeOverrideStore()
	r := newOverridesRouter(store)

	body := map[string]any{
		"requires_ultra":         true,
		"extra_aliases":          []string{"a", "b"},
		"note":                   "hi",
		"min_credits_hundredths": 12345,
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/models/nano-banana-2/override", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	if store.lastUpsert == nil {
		t.Fatal("no upsert captured")
	}
	if store.lastUpsert.Alias != "nano-banana-2" {
		t.Errorf("alias: got %q", store.lastUpsert.Alias)
	}
	if store.lastUpsert.RequiresUltra == nil || !*store.lastUpsert.RequiresUltra {
		t.Errorf("requires_ultra should be ptr(true), got %v", store.lastUpsert.RequiresUltra)
	}
	if store.lastUpsert.RequiresPaid != nil {
		t.Errorf("requires_paid should be nil (unset), got %v", store.lastUpsert.RequiresPaid)
	}
	if store.lastUpsert.MinCreditsHundredths == nil || *store.lastUpsert.MinCreditsHundredths != 12345 {
		t.Errorf("min_credits_hundredths: got %v", store.lastUpsert.MinCreditsHundredths)
	}
	if len(store.lastUpsert.ExtraAliases) != 2 {
		t.Errorf("extra_aliases: got %v", store.lastUpsert.ExtraAliases)
	}
	if store.lastUpsert.Note != "hi" {
		t.Errorf("note: got %q", store.lastUpsert.Note)
	}
}

func TestModelOverrides_Put_DedupsExtraAliases(t *testing.T) {
	store := newFakeOverrideStore()
	r := newOverridesRouter(store)

	body := map[string]any{"extra_aliases": []string{"a", "a", "", "  ", "b"}}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPut, "/models/x/override", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	got := store.lastUpsert.ExtraAliases
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("extra_aliases dedup: got %v, want [a b]", got)
	}
}

func TestModelOverrides_Get_NotFoundIsRenderedAs404(t *testing.T) {
	store := newFakeOverrideStore()
	r := newOverridesRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/models/ghost/override", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rec.Code)
	}
}

func TestModelOverrides_Delete_ClearsRow(t *testing.T) {
	store := newFakeOverrideStore()
	paid := true
	store.rows["x"] = &domain.ModelOverride{Alias: "x", RequiresPaid: &paid}

	r := newOverridesRouter(store)
	req := httptest.NewRequest(http.MethodDelete, "/models/x/override", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status: got %d, want 204", rec.Code)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "x" {
		t.Errorf("delete calls: %v", store.deleted)
	}
}

func TestModelOverrides_List_ReturnsRows(t *testing.T) {
	store := newFakeOverrideStore()
	ultra := true
	store.rows["a"] = &domain.ModelOverride{Alias: "a", RequiresUltra: &ultra, ExtraAliases: []string{"a-plus"}}

	r := newOverridesRouter(store)
	req := httptest.NewRequest(http.MethodGet, "/models/overrides", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	body, _ := io.ReadAll(rec.Body)
	var resp struct {
		Total int              `json:"total"`
		Data  []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("total: got %d, want 1", resp.Total)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data len: %d", len(resp.Data))
	}
	// Nil pointer fields must serialize as JSON null (not missing key)
	// so the client's tri-state select can distinguish "inherit" from
	// "explicit false".
	row := resp.Data[0]
	if row["alias"] != "a" {
		t.Errorf("alias: got %v", row["alias"])
	}
	if row["requires_paid"] != nil {
		t.Errorf("requires_paid should be null, got %v", row["requires_paid"])
	}
	if v, ok := row["requires_ultra"].(bool); !ok || !v {
		t.Errorf("requires_ultra: got %v", row["requires_ultra"])
	}
}
