package pollworker

// Markup-forwarding tests for the Worker. These verify the caller layer
// actually reads APIKey.MarkupPct from the APIKeyStore and passes it into
// Meter.OnJobTerminal. The Recorder's own math (markup 1.5 -> charged =
// actual * 1.5, and the 0 -> 1.0 fallback) is covered separately in
// metering_test.go; here we only care about the wiring.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeAPIKeyStore is a minimal stand-in for ports.APIKeyStore. Only Get is
// exercised by the worker; the other methods panic to keep the surface tight
// and surface accidental new dependencies loudly.
type fakeAPIKeyStore struct {
	byID map[string]*domain.APIKey
	err  error
}

func (s *fakeAPIKeyStore) Get(_ context.Context, id string) (*domain.APIKey, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.byID == nil {
		return nil, nil
	}
	return s.byID[id], nil
}

func (s *fakeAPIKeyStore) GetByHash(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) Create(context.Context, *domain.APIKey) error { panic("not implemented") }
func (s *fakeAPIKeyStore) Revoke(context.Context, string) error         { panic("not implemented") }
func (s *fakeAPIKeyStore) IncrementUsage(context.Context, string, int64) error {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) List(context.Context) ([]domain.APIKey, error) {
	panic("not implemented")
}
func (s *fakeAPIKeyStore) ListByCPAPartner(context.Context, string) ([]domain.APIKey, error) {
	panic("not implemented")
}

// pendingJob is a small helper — every test starts from the same base job,
// only the api_key_id and PreBalanceH vary. Keeping this here avoids
// hand-writing the same 10-field struct four times.
func pendingJob(id, apiKeyID string) domain.Job {
	return domain.Job{
		ID:            id,
		APIKeyID:      apiKeyID,
		AccountID:     "acc_markup",
		ModelAlias:    "seedance-2-0-mini",
		JST:           "text2video_seedance",
		UpstreamJobID: "upst_" + id,
		RequestTS:     time.Now().Add(-30 * time.Second),
		Status:        domain.JobQueued,
		PreBalanceH:   1000,
	}
}

// TestWorker_ForwardsMarkupFromAPIKey is the happy path: an APIKey row with
// MarkupPct=1.5 must reach the Meter as markupPct=1.5.
func TestWorker_ForwardsMarkupFromAPIKey(t *testing.T) {
	jobs := &fakeJobStore{pending: []domain.Job{pendingJob("job_mk", "key_mk")}}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_markup"}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_job_mk", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_job_mk", Status: "completed", ResultURL: "https://cdn/x.mp4"},
	}
	meter := &fakeMeter{}
	keys := &fakeAPIKeyStore{byID: map[string]*domain.APIKey{
		"key_mk": {ID: "key_mk", MarkupPct: 1.5},
	}}

	w := newWorker(t, jobs, accs, ups)
	w.Meter = meter
	w.APIKeys = keys

	w.tick(context.Background())

	if len(meter.calls) != 1 {
		t.Fatalf("expected 1 meter call, got %d", len(meter.calls))
	}
	if got := meter.calls[0].markup; got != 1.5 {
		t.Errorf("markup forwarded to meter: got %v want 1.5", got)
	}
}

// TestWorker_MarkupZeroWhenAPIKeyStoreNil guards the wire-not-attached
// deployment: Worker.APIKeys nil must produce markup=0 (Recorder maps that
// to multiplier 1.0). Without this the Recorder would still receive 0 today
// but the intent is explicit: no store, no markup.
func TestWorker_MarkupZeroWhenAPIKeyStoreNil(t *testing.T) {
	jobs := &fakeJobStore{pending: []domain.Job{pendingJob("job_nil", "key_any")}}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_markup"}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_job_nil", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_job_nil", Status: "completed"},
	}
	meter := &fakeMeter{}

	w := newWorker(t, jobs, accs, ups)
	w.Meter = meter
	// APIKeys deliberately left nil.

	w.tick(context.Background())

	if len(meter.calls) != 1 {
		t.Fatalf("expected 1 meter call, got %d", len(meter.calls))
	}
	if got := meter.calls[0].markup; got != 0 {
		t.Errorf("markup with nil APIKeys: got %v want 0", got)
	}
}

// TestWorker_MarkupZeroOnAPIKeyGetError verifies the fault-tolerant path:
// if the APIKeyStore Get errors (row revoked mid-flight, transient DB
// issue), the worker must fall back to markup=0 and NOT propagate the
// error — the terminal transition and metering event still succeed.
func TestWorker_MarkupZeroOnAPIKeyGetError(t *testing.T) {
	jobs := &fakeJobStore{pending: []domain.Job{pendingJob("job_err", "key_err")}}
	accs := &fakeAccountStore{acc: &domain.Account{ID: "acc_markup"}}
	ups := &fakeUpstream{
		status: &upstream.StatusResponse{ID: "upst_job_err", Status: "completed"},
		job:    &upstream.FetchResponse{ID: "upst_job_err", Status: "completed"},
	}
	meter := &fakeMeter{}
	keys := &fakeAPIKeyStore{err: errors.New("db down")}

	w := newWorker(t, jobs, accs, ups)
	w.Meter = meter
	w.APIKeys = keys

	w.tick(context.Background())

	if len(meter.calls) != 1 {
		t.Fatalf("expected 1 meter call, got %d", len(meter.calls))
	}
	if got := meter.calls[0].markup; got != 0 {
		t.Errorf("markup on Get error: got %v want 0 (fallback)", got)
	}
	// And the terminal transition still happened — otherwise metering ran
	// on a still-queued job, which the Recorder would refuse.
	if len(jobs.updates) == 0 {
		t.Errorf("expected terminal UpdateStatus despite APIKey lookup error")
	}
}

// Compile-time assertion: fakeAPIKeyStore satisfies ports.APIKeyStore. If
// the interface grows a method the test build fails loudly instead of
// silently drifting.
var _ ports.APIKeyStore = (*fakeAPIKeyStore)(nil)
