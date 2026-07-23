package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeUsageStore is a partial ports.UsageEventStore implementation that only
// covers Query and Aggregate. Insert panics so an accidental write from the
// handler surface breaks the test loudly.
type fakeUsageStore struct {
	// Query recording / stubs.
	queryRows []domain.UsageEvent
	queryErr  error
	lastQuery ports.UsageQuery
	queryHits int

	// Aggregate recording / stubs.
	aggRows []ports.UsageAggRow
	aggErr  error
	lastAgg ports.UsageAggQuery
	aggHits int
}

func (f *fakeUsageStore) Insert(context.Context, *domain.UsageEvent) error {
	panic("not implemented")
}

func (f *fakeUsageStore) Query(_ context.Context, q ports.UsageQuery) ([]domain.UsageEvent, error) {
	f.lastQuery = q
	f.queryHits++
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.queryRows, nil
}

func (f *fakeUsageStore) Aggregate(_ context.Context, q ports.UsageAggQuery) ([]ports.UsageAggRow, error) {
	f.lastAgg = q
	f.aggHits++
	if f.aggErr != nil {
		return nil, f.aggErr
	}
	return f.aggRows, nil
}

// SumChargedCreditsHForAccount is unused by the admin usage handler — panic
// so a mistaken call from that surface breaks the test loudly.
func (f *fakeUsageStore) SumChargedCreditsHForAccount(context.Context, string, time.Time, time.Time) (int64, error) {
	panic("not implemented")
}

// newUsageRouter mounts a UsageHandler onto a plain chi router so tests can
// exercise the full routing path (no auth middleware attached).
func newUsageRouter(store ports.UsageEventStore) chi.Router {
	r := chi.NewRouter()
	h := NewUsageHandler(store)
	h.Register(r)
	return r
}

func sampleUsageEvents() []domain.UsageEvent {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return []domain.UsageEvent{
		{ID: "usg_1", TS: base, APIKeyID: "key_1", AccountID: "acc_1", ModelAlias: "veo3", Status: domain.JobCompleted},
		{ID: "usg_2", TS: base.Add(time.Hour), APIKeyID: "key_1", AccountID: "acc_2", ModelAlias: "veo3", Status: domain.JobFailed},
		{ID: "usg_3", TS: base.Add(2 * time.Hour), APIKeyID: "key_2", AccountID: "acc_2", ModelAlias: "kling", Status: domain.JobCompleted},
	}
}

func TestUsageHandler_List_ReturnsRows(t *testing.T) {
	store := &fakeUsageStore{queryRows: sampleUsageEvents()}
	r := newUsageRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
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
	if got, want := body["limit"], float64(defaultUsageLimit); got != want {
		t.Errorf("limit: got %v want %v", got, want)
	}
	if got, want := body["offset"], float64(0); got != want {
		t.Errorf("offset: got %v want %v", got, want)
	}
}

func TestUsageHandler_List_ForwardsFilters(t *testing.T) {
	store := &fakeUsageStore{}
	r := newUsageRouter(store)

	req := httptest.NewRequest(http.MethodGet,
		"/usage?api_key_id=key_1&model_alias=veo3&since=2026-01-01T00:00:00Z&limit=50&offset=10",
		nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastQuery.APIKeyID, "key_1"; got != want {
		t.Errorf("filter.APIKeyID: got %q want %q", got, want)
	}
	if got, want := store.lastQuery.ModelAlias, "veo3"; got != want {
		t.Errorf("filter.ModelAlias: got %q want %q", got, want)
	}
	if got, want := store.lastQuery.Limit, 50; got != want {
		t.Errorf("filter.Limit: got %d want %d", got, want)
	}
	if got, want := store.lastQuery.Offset, 10; got != want {
		t.Errorf("filter.Offset: got %d want %d", got, want)
	}
	wantSince := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !store.lastQuery.Since.Equal(wantSince) {
		t.Errorf("filter.Since: got %v want %v", store.lastQuery.Since, wantSince)
	}
}

func TestUsageHandler_List_LimitCap(t *testing.T) {
	store := &fakeUsageStore{}
	r := newUsageRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/usage?limit=99999", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastQuery.Limit, maxUsageLimit; got != want {
		t.Errorf("filter.Limit cap: got %d want %d", got, want)
	}
}

func TestUsageHandler_List_InvalidLimit(t *testing.T) {
	store := &fakeUsageStore{}
	r := newUsageRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/usage?limit=abc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.queryHits != 0 {
		t.Errorf("Query must not be called when limit invalid; got hits=%d", store.queryHits)
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

func TestUsageHandler_List_InvalidSince(t *testing.T) {
	store := &fakeUsageStore{}
	r := newUsageRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/usage?since=notatime", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.queryHits != 0 {
		t.Errorf("Query must not be called when since invalid; got hits=%d", store.queryHits)
	}
}

func TestUsageHandler_Aggregate_GroupByWhitelist(t *testing.T) {
	store := &fakeUsageStore{}
	r := newUsageRouter(store)

	// "password" is not on the whitelist and must be silently dropped.
	req := httptest.NewRequest(http.MethodGet,
		"/usage/aggregate?group_by=api_key_id,password,model_alias", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := store.lastAgg.GroupBy
	want := []string{"api_key_id", "model_alias"}
	if len(got) != len(want) {
		t.Fatalf("GroupBy len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("GroupBy[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestUsageHandler_Aggregate_ReturnsRows(t *testing.T) {
	store := &fakeUsageStore{
		aggRows: []ports.UsageAggRow{
			{Keys: map[string]string{"model_alias": "veo3"}, RequestCount: 10, CompletedCount: 9},
			{Keys: map[string]string{"model_alias": "kling"}, RequestCount: 5, CompletedCount: 5},
		},
	}
	r := newUsageRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/usage/aggregate?group_by=model_alias", nil)
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
	if got, want := len(data), 2; got != want {
		t.Errorf("data len: got %d want %d", got, want)
	}
}

func TestUsageHandler_StoreError(t *testing.T) {
	store := &fakeUsageStore{queryErr: errors.New("boom")}
	r := newUsageRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/usage", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body.error missing or wrong type: %T", body["error"])
	}
	if errObj["type"] != "internal" {
		t.Errorf("error.type: got %v want internal", errObj["type"])
	}
}
