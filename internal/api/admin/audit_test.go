package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeAuditStore is a partial ports.AuditStore implementation. Only
// List is exercised by the handler tests; Insert panics so an
// unexpected write from the handler surface breaks the test loudly.
type fakeAuditStore struct {
	rows      []domain.AuditEvent
	lastQuery ports.AuditFilter
	hits      int
	err       error
}

func (f *fakeAuditStore) Insert(context.Context, *domain.AuditEvent) error {
	panic("not implemented")
}

func (f *fakeAuditStore) List(_ context.Context, filter ports.AuditFilter) ([]domain.AuditEvent, error) {
	f.lastQuery = filter
	f.hits++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func newAuditRouterForAdmin(store ports.AuditStore) chi.Router {
	r := chi.NewRouter()
	NewAuditHandler(store).Register(r)
	return r
}

func sampleAuditEvents() []domain.AuditEvent {
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	return []domain.AuditEvent{
		{ID: "audit_1", TS: base.Add(2 * time.Hour), Actor: "sk-hg-aa", Method: "POST", Path: "/admin/keys",
			Route: "/keys", Status: 201, ResourceType: "apikey", BodyHash: "hash1"},
		{ID: "audit_2", TS: base.Add(time.Hour), Actor: "sk-hg-bb", Method: "DELETE", Path: "/admin/accounts/acc_1",
			Route: "/accounts/{id}", Status: 200, ResourceType: "account", ResourceID: "acc_1"},
		{ID: "audit_3", TS: base, Actor: "sk-hg-aa", Method: "POST", Path: "/admin/jobs/purge",
			Route: "/jobs/purge", Status: 200, ResourceType: "job"},
	}
}

func TestAuditHandler_List_ReturnsRows(t *testing.T) {
	store := &fakeAuditStore{rows: sampleAuditEvents()}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("body.data missing or wrong type: %T", body["data"])
	}
	if got, want := len(data), 3; got != want {
		t.Errorf("data len: got %d want %d", got, want)
	}
	// Default limit surfaces on the response envelope.
	if got, want := body["limit"], float64(defaultAuditLimit); got != want {
		t.Errorf("limit: got %v want %v", got, want)
	}
	if got, want := body["offset"], float64(0); got != want {
		t.Errorf("offset: got %v want %v", got, want)
	}

	// First row should carry all the documented JSON keys so downstream
	// operator tooling knows what to bind to.
	first, _ := data[0].(map[string]any)
	for _, key := range []string{
		"id", "ts", "actor", "method", "path", "route", "status",
		"resource_type", "resource_id", "body_hash", "error_detail",
	} {
		if _, present := first[key]; !present {
			t.Errorf("missing key %q in row: %v", key, first)
		}
	}
}

func TestAuditHandler_List_ActorFilter(t *testing.T) {
	store := &fakeAuditStore{}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet,
		"/audit?actor=sk-hg-aa&resource_type=apikey&resource_id=key_1&method=POST&limit=25&offset=5&since=2026-01-01T00:00:00Z",
		nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastQuery.Actor, "sk-hg-aa"; got != want {
		t.Errorf("filter.Actor: got %q want %q", got, want)
	}
	if got, want := store.lastQuery.ResourceType, "apikey"; got != want {
		t.Errorf("filter.ResourceType: got %q want %q", got, want)
	}
	if got, want := store.lastQuery.ResourceID, "key_1"; got != want {
		t.Errorf("filter.ResourceID: got %q want %q", got, want)
	}
	if got, want := store.lastQuery.Method, "POST"; got != want {
		t.Errorf("filter.Method: got %q want %q", got, want)
	}
	if got, want := store.lastQuery.Limit, 25; got != want {
		t.Errorf("filter.Limit: got %d want %d", got, want)
	}
	if got, want := store.lastQuery.Offset, 5; got != want {
		t.Errorf("filter.Offset: got %d want %d", got, want)
	}
	wantSince := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !store.lastQuery.Since.Equal(wantSince) {
		t.Errorf("filter.Since: got %v want %v", store.lastQuery.Since, wantSince)
	}
}

func TestAuditHandler_List_InvalidSince(t *testing.T) {
	store := &fakeAuditStore{}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit?since=notatime", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.hits != 0 {
		t.Errorf("List must not be called when since invalid; got hits=%d", store.hits)
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

func TestAuditHandler_List_LimitCap(t *testing.T) {
	store := &fakeAuditStore{}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit?limit=99999", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastQuery.Limit, maxAuditLimit; got != want {
		t.Errorf("filter.Limit cap: got %d want %d", got, want)
	}
}
