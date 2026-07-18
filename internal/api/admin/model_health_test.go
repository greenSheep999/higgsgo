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

// fakeHealthStore is a minimal ports.ModelHealthStore for ModelHealthHandler
// tests. Insert panics so an accidental write path in the handler surface
// breaks the test loudly.
type fakeHealthStore struct {
	// List behavior.
	listRows []ports.ModelHealthRow
	listErr  error
	listHits int

	// Latest behavior.
	latestRow  *ports.ModelHealthRow
	latestErr  error
	lastGetJST string
}

func (f *fakeHealthStore) Insert(
	context.Context, string, time.Time, domain.JobStatus, int, int64, int,
) error {
	panic("not implemented")
}

func (f *fakeHealthStore) Latest(_ context.Context, jst string) (*ports.ModelHealthRow, error) {
	f.lastGetJST = jst
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	return f.latestRow, nil
}

func (f *fakeHealthStore) List(context.Context) ([]ports.ModelHealthRow, error) {
	f.listHits++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRows, nil
}

func (f *fakeHealthStore) UptimeByJST(_ context.Context, _ time.Time) (map[string]float64, error) {
	// Return uptime derived from listRows for test convenience.
	m := make(map[string]float64)
	totals := make(map[string]int)
	oks := make(map[string]int)
	for _, r := range f.listRows {
		totals[r.JST]++
		if r.Verdict == "completed" {
			oks[r.JST]++
		}
	}
	for jst, total := range totals {
		m[jst] = float64(oks[jst]) / float64(total) * 100.0
	}
	return m, nil
}

// Compile-time assertion: fakeHealthStore must satisfy ports.ModelHealthStore
// so a future interface change breaks this file rather than silently making
// the handler tests exercise a stale surface.
var _ ports.ModelHealthStore = (*fakeHealthStore)(nil)

// newModelHealthRouter mounts a ModelHealthHandler onto a plain chi router so
// tests can exercise the full routing path (no auth middleware attached).
func newModelHealthRouter(store ports.ModelHealthStore) chi.Router {
	r := chi.NewRouter()
	h := NewModelHealthHandler(store)
	h.Register(r)
	return r
}

func sampleHealthRows() []ports.ModelHealthRow {
	base := time.Date(2026, 7, 17, 6, 0, 0, 0, time.UTC)
	return []ports.ModelHealthRow{
		{JST: "seedance_2_0", CheckedAt: base.Add(2 * time.Hour), Verdict: domain.JobCompleted, HTTPStatus: 200, Cost: 1800, PollTimeSec: 20},
		{JST: "veo3_1", CheckedAt: base.Add(1 * time.Hour), Verdict: domain.JobFailed, HTTPStatus: 500, Cost: 0, PollTimeSec: 5},
	}
}

func TestModelHealthHandler_List_ReturnsData(t *testing.T) {
	store := &fakeHealthStore{listRows: sampleHealthRows()}
	r := newModelHealthRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/model-health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.listHits != 1 {
		t.Errorf("List call count: got %d want 1", store.listHits)
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
	// First row must be the seedance row (newest checked_at is returned
	// first by the store; the handler must preserve that order).
	first, ok := data[0].(map[string]any)
	if !ok {
		t.Fatalf("data[0] type: %T", data[0])
	}
	if first["jst"] != "seedance_2_0" {
		t.Errorf("data[0].jst: got %v want seedance_2_0", first["jst"])
	}
	if first["verdict"] != "completed" {
		t.Errorf("data[0].verdict: got %v want completed", first["verdict"])
	}
	// checked_at should be an RFC3339 string.
	ts, ok := first["checked_at"].(string)
	if !ok {
		t.Errorf("data[0].checked_at type: %T", first["checked_at"])
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("data[0].checked_at not RFC3339: %v (%q)", err, ts)
	}
}

func TestModelHealthHandler_List_VerdictFilter(t *testing.T) {
	// Three rows: two "completed", one "failed". ?verdict=completed must
	// return only the two matching rows.
	base := time.Date(2026, 7, 17, 6, 0, 0, 0, time.UTC)
	rows := []ports.ModelHealthRow{
		{JST: "seedance_2_0", CheckedAt: base.Add(2 * time.Hour), Verdict: domain.JobCompleted},
		{JST: "veo3_1", CheckedAt: base.Add(1 * time.Hour), Verdict: domain.JobFailed},
		{JST: "kling_2_6", CheckedAt: base, Verdict: domain.JobCompleted},
	}
	store := &fakeHealthStore{listRows: rows}
	r := newModelHealthRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/model-health?verdict=completed", nil)
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
		t.Fatalf("data len: got %d want %d; body=%s", got, want, rec.Body.String())
	}
	for i, raw := range data {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("data[%d] type: %T", i, raw)
		}
		if row["verdict"] != "completed" {
			t.Errorf("data[%d].verdict: got %v want completed", i, row["verdict"])
		}
	}
}

func TestModelHealthHandler_List_StaleBefore(t *testing.T) {
	// Three rows at t, t+1h, t+2h. ?stale_before=t+1h30m returns only
	// rows strictly before that cutoff (t and t+1h).
	base := time.Date(2026, 7, 17, 6, 0, 0, 0, time.UTC)
	rows := []ports.ModelHealthRow{
		{JST: "newest", CheckedAt: base.Add(2 * time.Hour), Verdict: domain.JobCompleted},
		{JST: "middle", CheckedAt: base.Add(1 * time.Hour), Verdict: domain.JobCompleted},
		{JST: "oldest", CheckedAt: base, Verdict: domain.JobCompleted},
	}
	store := &fakeHealthStore{listRows: rows}
	r := newModelHealthRouter(store)

	cutoff := base.Add(90 * time.Minute).UTC().Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/model-health?stale_before="+cutoff, nil)
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
		t.Fatalf("data len: got %d want %d; body=%s", got, want, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, raw := range data {
		row := raw.(map[string]any)
		seen[row["jst"].(string)] = true
	}
	if !seen["middle"] || !seen["oldest"] {
		t.Errorf("expected middle+oldest, got %v", seen)
	}
	if seen["newest"] {
		t.Errorf("newest row leaked past stale_before cutoff")
	}
}

func TestModelHealthHandler_List_InvalidStaleBefore(t *testing.T) {
	// A malformed stale_before value must produce a 400 without calling
	// the store, so we can distinguish "user error" from "empty result".
	store := &fakeHealthStore{listRows: sampleHealthRows()}
	r := newModelHealthRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/model-health?stale_before=notatime", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.listHits != 0 {
		t.Errorf("List must not be called on invalid stale_before; hits=%d", store.listHits)
	}
}

func TestModelHealthHandler_Get_Success(t *testing.T) {
	row := &ports.ModelHealthRow{
		JST:         "text2image_soul",
		CheckedAt:   time.Date(2026, 7, 17, 6, 0, 0, 0, time.UTC),
		Verdict:     domain.JobCompleted,
		HTTPStatus:  200,
		Cost:        900,
		PollTimeSec: 12,
	}
	store := &fakeHealthStore{latestRow: row}
	r := newModelHealthRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/model-health/text2image_soul", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.lastGetJST != "text2image_soul" {
		t.Errorf("lastGetJST: got %q want text2image_soul", store.lastGetJST)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["jst"] != "text2image_soul" {
		t.Errorf("body.jst: got %v want text2image_soul", body["jst"])
	}
	if body["verdict"] != "completed" {
		t.Errorf("body.verdict: got %v want completed", body["verdict"])
	}
	if body["http_status"] != float64(200) {
		t.Errorf("body.http_status: got %v want 200", body["http_status"])
	}
	if body["cost"] != float64(900) {
		t.Errorf("body.cost: got %v want 900", body["cost"])
	}
	if body["poll_time_sec"] != float64(12) {
		t.Errorf("body.poll_time_sec: got %v want 12", body["poll_time_sec"])
	}
}

func TestModelHealthHandler_Get_NotFound(t *testing.T) {
	// Latest returning (nil, nil) is the store's contract for "never
	// probed". The handler must translate that to 404 rather than 500 or
	// an empty 200 body.
	store := &fakeHealthStore{latestRow: nil}
	r := newModelHealthRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/model-health/never_probed", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("body.error missing or wrong type: %T", body["error"])
	}
	if errObj["type"] != "not_found" {
		t.Errorf("error.type: got %v want not_found", errObj["type"])
	}
}
