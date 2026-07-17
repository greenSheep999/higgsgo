package v1

// Tests for GET /v1/jobs (HandleJobsList):
//
//   - basic response shape and default limit
//   - limit capping at maxJobsListLimit
//   - invalid since= 400 handling
//   - internal fields (pre_balance_h) never leak into the JSON body

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeJobStoreForList is a minimal ports.JobStore stub that only implements
// ListByAPIKey. The other methods panic loudly so accidental use from a
// list-handler test surfaces immediately.
type fakeJobStoreForList struct {
	rows      []domain.Job
	err       error
	lastKey   string
	lastFilt  ports.JobFilter
	callCount int
}

func (f *fakeJobStoreForList) Create(context.Context, *domain.Job) error {
	panic("not implemented")
}

func (f *fakeJobStoreForList) UpdateStatus(context.Context, string, domain.JobStatus, ports.JobMeta) error {
	panic("not implemented")
}

func (f *fakeJobStoreForList) Get(context.Context, string) (*domain.Job, error) {
	panic("not implemented")
}

func (f *fakeJobStoreForList) ListPending(context.Context) ([]domain.Job, error) {
	panic("not implemented")
}

func (f *fakeJobStoreForList) ListByAPIKey(_ context.Context, apiKeyID string, filter ports.JobFilter) ([]domain.Job, error) {
	f.lastKey = apiKeyID
	f.lastFilt = filter
	f.callCount++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// newJobsListRouter mounts HandleJobsList onto a chi router, injecting a
// stub APIKey into the request context so the handler's caller-scoping
// path (which reads middleware.APIKeyFromContext) has something to work
// with.
func newJobsListRouter(t *testing.T, jobs ports.JobStore) chi.Router {
	t.Helper()
	h := &Handler{Jobs: jobs}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := middleware.ContextWithAPIKey(req.Context(), &domain.APIKey{
				ID:     "key_test",
				Status: "active",
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Get("/v1/jobs", h.HandleJobsList)
	return r
}

func sampleJobs() []domain.Job {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return []domain.Job{
		{
			ID:            "job_1",
			APIKeyID:      "key_test",
			ModelAlias:    "seedance-2-0-mini",
			JST:           "text2video_seedance",
			RequestTS:     base.Add(time.Hour),
			Status:        domain.JobCompleted,
			UpstreamJobID: "up_1",
			ResultURL:     "https://example.com/1.mp4",
		},
		{
			ID:         "job_2",
			APIKeyID:   "key_test",
			ModelAlias: "seedance-2-0-mini",
			JST:        "text2video_seedance",
			RequestTS:  base,
			Status:     domain.JobFailed,
		},
	}
}

func TestListJobs_Basic(t *testing.T) {
	store := &fakeJobStoreForList{rows: sampleJobs()}
	r := newJobsListRouter(t, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
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
	if got, want := body["limit"], float64(defaultJobsListLimit); got != want {
		t.Errorf("limit: got %v want %v", got, want)
	}
	if got, want := body["offset"], float64(0); got != want {
		t.Errorf("offset: got %v want %v", got, want)
	}
	if got, want := store.lastKey, "key_test"; got != want {
		t.Errorf("store lastKey: got %q want %q", got, want)
	}
}

func TestListJobs_LimitCap(t *testing.T) {
	store := &fakeJobStoreForList{}
	r := newJobsListRouter(t, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?limit=99999", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastFilt.Limit, maxJobsListLimit; got != want {
		t.Errorf("filter.Limit cap: got %d want %d", got, want)
	}
}

func TestListJobs_InvalidSince(t *testing.T) {
	store := &fakeJobStoreForList{}
	r := newJobsListRouter(t, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?since=notatime", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rec.Code, rec.Body.String())
	}
	if store.callCount != 0 {
		t.Errorf("store must not be hit when since is invalid; got calls=%d", store.callCount)
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

// TestListJobs_HidesPreBalance guards the contract that internal-only
// accounting fields never surface to the caller. A job with PreBalanceH
// set to a distinctive value round-trips through the handler and its
// value must not appear anywhere in the response bytes — either as the
// column name or as the raw number.
func TestListJobs_HidesPreBalance(t *testing.T) {
	rows := []domain.Job{
		{
			ID:          "job_pb",
			APIKeyID:    "key_test",
			ModelAlias:  "seedance-2-0-mini",
			JST:         "text2video_seedance",
			RequestTS:   time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			Status:      domain.JobCompleted,
			PreBalanceH: 99999,
		},
	}
	store := &fakeJobStoreForList{rows: rows}
	r := newJobsListRouter(t, store)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "pre_balance_h") {
		t.Errorf("response leaked pre_balance_h column name; body=%s", body)
	}
	if strings.Contains(body, "99999") {
		t.Errorf("response leaked pre_balance_h value; body=%s", body)
	}
}
