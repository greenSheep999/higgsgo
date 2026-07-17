package admin

// Tests for /admin/jobs (JobsHandler):
//
//   - List with no filter returns everything the store hands back
//   - List with status= narrows to the requested status
//   - List with account_id= narrows to the requested account
//   - limit=99999 is capped by the handler at maxAdminJobsLimit before
//     the store sees it
//   - Get returns the admin view which includes internal fields
//     (pre_balance_h, actual_credits_h, charged_credits_h) that the
//     public /v1/jobs surface hides
//   - Get on an unknown id returns 404 with a not_found error envelope

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

// fakeAdminJobStore is a partial ports.JobStore that only implements the
// methods the /admin/jobs handler exercises. Every other method panics so
// a silent behaviour change in the handler surface is caught immediately.
type fakeAdminJobStore struct {
	// ListAll behaviour.
	listRows      []domain.Job
	listErr       error
	lastFilter    ports.JobFilter
	listCallCount int

	// Get behaviour.
	getResult *domain.Job
	getErr    error
	lastGetID string
}

func (f *fakeAdminJobStore) ListAll(_ context.Context, filter ports.JobFilter) ([]domain.Job, error) {
	f.lastFilter = filter
	f.listCallCount++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRows, nil
}

func (f *fakeAdminJobStore) Get(_ context.Context, id string) (*domain.Job, error) {
	f.lastGetID = id
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

// Methods below are not touched by the admin handler; they panic so any
// accidental new call site surfaces during tests.
func (f *fakeAdminJobStore) Create(context.Context, *domain.Job) error {
	panic("not implemented")
}

func (f *fakeAdminJobStore) UpdateStatus(context.Context, string, domain.JobStatus, ports.JobMeta) error {
	panic("not implemented")
}

func (f *fakeAdminJobStore) ListPending(context.Context) ([]domain.Job, error) {
	panic("not implemented")
}

func (f *fakeAdminJobStore) ListByAPIKey(context.Context, string, ports.JobFilter) ([]domain.Job, error) {
	panic("not implemented")
}

// newAdminJobsRouter builds a chi router with the JobsHandler mounted so
// tests exercise the real routing surface end-to-end.
func newAdminJobsRouter(store ports.JobStore) chi.Router {
	r := chi.NewRouter()
	h := NewJobsHandler(store)
	h.Register(r)
	return r
}

// sampleAdminJobs returns two representative jobs: one completed with full
// accounting, one failed with an error attached.
func sampleAdminJobs() []domain.Job {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return []domain.Job{
		{
			ID:                       "job_a1",
			APIKeyID:                 "key_a",
			AccountID:                "acc_1",
			GroupID:                  "grp_1",
			ModelAlias:               "seedance-2-0-mini",
			JST:                      "text2video_seedance",
			Endpoint:                 "/jobs/v2/seedance_2_0",
			RequestTS:                base.Add(time.Hour),
			Status:                   domain.JobCompleted,
			UpstreamJobID:            "up_1",
			ResultURL:                "https://example.com/1.mp4",
			PreBalanceH:              123456,
			ActualCreditsHundredths:  100,
			ChargedCreditsHundredths: 150,
		},
		{
			ID:          "job_a2",
			APIKeyID:    "key_b",
			AccountID:   "acc_2",
			ModelAlias:  "seedance-2-0-mini",
			JST:         "text2video_seedance",
			RequestTS:   base,
			Status:      domain.JobFailed,
			ErrorType:   domain.ErrUpstream,
			ErrorDetail: "upstream said no",
		},
	}
}

func TestAdminJobsHandler_List_NoFilter(t *testing.T) {
	store := &fakeAdminJobStore{listRows: sampleAdminJobs()}
	r := newAdminJobsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rec.Body.String())
	}
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("body.data missing or wrong type: %T", body["data"])
	}
	if got, want := len(data), 2; got != want {
		t.Errorf("data len: got %d want %d", got, want)
	}
	// With no query params, every filter dimension must be zero-valued so
	// the store sees "return everything".
	if store.lastFilter.Status != "" {
		t.Errorf("filter.Status: got %q want empty", store.lastFilter.Status)
	}
	if store.lastFilter.AccountID != "" {
		t.Errorf("filter.AccountID: got %q want empty", store.lastFilter.AccountID)
	}
	if store.lastFilter.APIKeyID != "" {
		t.Errorf("filter.APIKeyID: got %q want empty", store.lastFilter.APIKeyID)
	}
	if store.lastFilter.GroupID != "" {
		t.Errorf("filter.GroupID: got %q want empty", store.lastFilter.GroupID)
	}
	if store.lastFilter.ModelAlias != "" {
		t.Errorf("filter.ModelAlias: got %q want empty", store.lastFilter.ModelAlias)
	}
	if got, want := store.lastFilter.Limit, defaultAdminJobsLimit; got != want {
		t.Errorf("filter.Limit default: got %d want %d", got, want)
	}
}

func TestAdminJobsHandler_List_StatusFilter(t *testing.T) {
	store := &fakeAdminJobStore{listRows: sampleAdminJobs()[:1]}
	r := newAdminJobsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/jobs?status=completed", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastFilter.Status, domain.JobCompleted; got != want {
		t.Errorf("filter.Status: got %q want %q", got, want)
	}
	if store.listCallCount != 1 {
		t.Errorf("ListAll calls: got %d want 1", store.listCallCount)
	}
}

func TestAdminJobsHandler_List_AccountIDFilter(t *testing.T) {
	store := &fakeAdminJobStore{listRows: sampleAdminJobs()[:1]}
	r := newAdminJobsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/jobs?account_id=acc_1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastFilter.AccountID, "acc_1"; got != want {
		t.Errorf("filter.AccountID: got %q want %q", got, want)
	}
}

func TestAdminJobsHandler_List_LimitCap(t *testing.T) {
	store := &fakeAdminJobStore{}
	r := newAdminJobsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/jobs?limit=99999", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got, want := store.lastFilter.Limit, maxAdminJobsLimit; got != want {
		t.Errorf("filter.Limit cap: got %d want %d", got, want)
	}
}

func TestAdminJobsHandler_Get_Success(t *testing.T) {
	rows := sampleAdminJobs()
	store := &fakeAdminJobStore{getResult: &rows[0]}
	r := newAdminJobsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/jobs/job_a1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if store.lastGetID != "job_a1" {
		t.Errorf("lastGetID: got %q want job_a1", store.lastGetID)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, rec.Body.String())
	}
	// Admin surface must carry the internal accounting fields the public
	// /v1 view hides. If any of these ever go missing, the operator can no
	// longer reconcile actual vs charged credit consumption.
	for _, key := range []string{
		"account_id", "group_id", "api_key_id",
		"pre_balance_h", "actual_credits_h", "charged_credits_h",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("admin view missing key %q; body=%v", key, body)
		}
	}
	if got, want := body["pre_balance_h"], float64(123456); got != want {
		t.Errorf("body.pre_balance_h: got %v want %v", got, want)
	}
	if got, want := body["actual_credits_h"], float64(100); got != want {
		t.Errorf("body.actual_credits_h: got %v want %v", got, want)
	}
}

func TestAdminJobsHandler_Get_NotFound(t *testing.T) {
	store := &fakeAdminJobStore{getErr: domain.ErrJobNotFound}
	r := newAdminJobsRouter(store)

	req := httptest.NewRequest(http.MethodGet, "/jobs/missing", nil)
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
