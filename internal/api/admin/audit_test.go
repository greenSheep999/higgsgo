package admin

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	queries   []ports.AuditFilter
	hits      int
	err       error
	// listFn overrides the default "return rows" behaviour. When set,
	// each call to List is dispatched to listFn — useful for exercising
	// paged / batched read paths where the returned slice depends on
	// filter.Offset.
	listFn func(filter ports.AuditFilter) ([]domain.AuditEvent, error)
}

func (f *fakeAuditStore) Insert(context.Context, *domain.AuditEvent) error {
	panic("not implemented")
}

func (f *fakeAuditStore) List(_ context.Context, filter ports.AuditFilter) ([]domain.AuditEvent, error) {
	f.lastQuery = filter
	f.queries = append(f.queries, filter)
	f.hits++
	if f.err != nil {
		return nil, f.err
	}
	if f.listFn != nil {
		return f.listFn(filter)
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

// --- Export ---------------------------------------------------------------

func TestAuditHandler_Export_JSONL(t *testing.T) {
	store := &fakeAuditStore{rows: sampleAuditEvents()}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit/export?format=jsonl", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/x-ndjson"; got != want {
		t.Errorf("Content-Type: got %q want %q", got, want)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment; filename=") {
		t.Errorf("Content-Disposition: got %q want attachment prefix", cd)
	}
	if !strings.HasSuffix(cd, ".jsonl") {
		t.Errorf("Content-Disposition: got %q want .jsonl suffix", cd)
	}

	lines := splitJSONLines(t, rec.Body.String())
	if got, want := len(lines), 3; got != want {
		t.Fatalf("line count: got %d want %d; body=%q", got, want, rec.Body.String())
	}
	// Each line must decode as an object with the documented keys so
	// downstream operator tooling can bind to a stable schema.
	for i, line := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("line %d: decode: %v; raw=%q", i, err, line)
		}
		for _, key := range []string{
			"id", "ts", "actor", "method", "path", "route", "status",
			"resource_type", "resource_id", "body_hash", "error_detail",
		} {
			if _, present := row[key]; !present {
				t.Errorf("line %d missing key %q: %v", i, key, row)
			}
		}
	}
}

func TestAuditHandler_Export_CSV(t *testing.T) {
	store := &fakeAuditStore{rows: sampleAuditEvents()}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit/export?format=csv", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "text/csv; charset=utf-8"; got != want {
		t.Errorf("Content-Type: got %q want %q", got, want)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasSuffix(got, ".csv") {
		t.Errorf("Content-Disposition: got %q want .csv suffix", got)
	}

	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv decode: %v; body=%q", err, rec.Body.String())
	}
	if got, want := len(records), 4; got != want { // header + 3 rows
		t.Fatalf("csv rows: got %d want %d; records=%v", got, want, records)
	}
	wantHeader := []string{
		"id", "ts", "actor", "method", "path", "route",
		"status", "resource_type", "resource_id", "body_hash", "error_detail",
	}
	for i, col := range wantHeader {
		if records[0][i] != col {
			t.Errorf("header col %d: got %q want %q", i, records[0][i], col)
		}
	}
	// Row 0 must line up with sampleAuditEvents()[0].
	if records[1][0] != "audit_1" {
		t.Errorf("row 0 id: got %q want %q", records[1][0], "audit_1")
	}
	if records[1][3] != "POST" {
		t.Errorf("row 0 method: got %q want %q", records[1][3], "POST")
	}
	if records[1][6] != "201" {
		t.Errorf("row 0 status: got %q want %q", records[1][6], "201")
	}
}

func TestAuditHandler_Export_DefaultsToJSONL(t *testing.T) {
	store := &fakeAuditStore{rows: sampleAuditEvents()}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit/export", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/x-ndjson"; got != want {
		t.Errorf("Content-Type: got %q want %q", got, want)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasSuffix(got, ".jsonl") {
		t.Errorf("Content-Disposition: got %q want .jsonl suffix", got)
	}
}

func TestAuditHandler_Export_UnknownFormat(t *testing.T) {
	store := &fakeAuditStore{}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit/export?format=xml", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.hits != 0 {
		t.Errorf("List must not be called on unknown format; got hits=%d", store.hits)
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

func TestAuditHandler_Export_FilterForwarded(t *testing.T) {
	store := &fakeAuditStore{rows: sampleAuditEvents()}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet,
		"/audit/export?format=jsonl&actor=sk-hg-aa&resource_type=apikey&since=2026-01-01T00:00:00Z&until=2026-02-01T00:00:00Z&method=POST",
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
	if got, want := store.lastQuery.Method, "POST"; got != want {
		t.Errorf("filter.Method: got %q want %q", got, want)
	}
	wantSince := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !store.lastQuery.Since.Equal(wantSince) {
		t.Errorf("filter.Since: got %v want %v", store.lastQuery.Since, wantSince)
	}
	wantUntil := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	if !store.lastQuery.Until.Equal(wantUntil) {
		t.Errorf("filter.Until: got %v want %v", store.lastQuery.Until, wantUntil)
	}
	// Filename should embed the since/until range when both bounds are
	// supplied so exports do not collide when saved side-by-side.
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "20260101T000000Z-20260201T000000Z") {
		t.Errorf("Content-Disposition: got %q want since-until range", got)
	}
}

func TestAuditHandler_Export_LargeDatasetBatching(t *testing.T) {
	// Fake store returns 500 rows for the first two offsets and 300
	// for the third. The handler must walk all three batches without
	// dropping or duplicating rows.
	const (
		batch1 = 500
		batch2 = 500
		batch3 = 300
	)
	makeRows := func(prefix string, n int) []domain.AuditEvent {
		out := make([]domain.AuditEvent, n)
		base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
		for i := 0; i < n; i++ {
			out[i] = domain.AuditEvent{
				ID:     fmt.Sprintf("%s_%d", prefix, i),
				TS:     base.Add(time.Duration(i) * time.Second),
				Actor:  "sk-hg-aa",
				Method: "POST",
				Path:   "/admin/keys",
				Route:  "/keys",
				Status: 201,
			}
		}
		return out
	}
	store := &fakeAuditStore{
		listFn: func(f ports.AuditFilter) ([]domain.AuditEvent, error) {
			switch f.Offset {
			case 0:
				return makeRows("a", batch1), nil
			case batch1:
				return makeRows("b", batch2), nil
			case batch1 + batch2:
				return makeRows("c", batch3), nil
			}
			return nil, nil
		},
	}
	r := newAuditRouterForAdmin(store)

	req := httptest.NewRequest(http.MethodGet, "/audit/export?format=jsonl", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String()[:min(len(rec.Body.String()), 200)])
	}
	lines := splitJSONLines(t, rec.Body.String())
	if got, want := len(lines), batch1+batch2+batch3; got != want {
		t.Fatalf("line count: got %d want %d", got, want)
	}
	if store.hits != 3 {
		t.Errorf("expected 3 List calls; got %d", store.hits)
	}
	// Each call should have advanced the offset by the previous batch
	// size so no row is double-counted.
	wantOffsets := []int{0, batch1, batch1 + batch2}
	for i, want := range wantOffsets {
		if got := store.queries[i].Offset; got != want {
			t.Errorf("List call %d: offset got %d want %d", i, got, want)
		}
	}
	// Spot-check the first and last rows to confirm ordering follows
	// the batch traversal, not something like a random shuffle.
	var firstRow map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &firstRow); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if firstRow["id"] != "a_0" {
		t.Errorf("first id: got %v want a_0", firstRow["id"])
	}
	var lastRow map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &lastRow); err != nil {
		t.Fatalf("decode last: %v", err)
	}
	if lastRow["id"] != fmt.Sprintf("c_%d", batch3-1) {
		t.Errorf("last id: got %v want c_%d", lastRow["id"], batch3-1)
	}
}

// splitJSONLines splits a JSONL body into per-line records, stripping
// the trailing newline that json.Encoder writes after each object.
func splitJSONLines(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		t.Fatalf("scan jsonl: %v", err)
	}
	return out
}
