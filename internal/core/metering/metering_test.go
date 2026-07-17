package metering

// Tests for Recorder.OnJobTerminal. Uses in-package fake implementations of
// UsageEventStore and APIKeyStore so the tests stay hermetic and quick — no
// SQLite migrations needed. AccountStore is unused by the recorder so we
// only supply what OnJobTerminal actually reads.

import (
	"context"
	"errors"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeEventStore records every Insert call. Query and Aggregate are not
// exercised here — the sqlite package has its own coverage for those.
type fakeEventStore struct {
	events  []domain.UsageEvent
	failErr error
}

func (s *fakeEventStore) Insert(_ context.Context, e *domain.UsageEvent) error {
	if s.failErr != nil {
		return s.failErr
	}
	s.events = append(s.events, *e)
	return nil
}

func (s *fakeEventStore) Query(context.Context, ports.UsageQuery) ([]domain.UsageEvent, error) {
	panic("not implemented")
}

func (s *fakeEventStore) Aggregate(context.Context, ports.UsageAggQuery) ([]ports.UsageAggRow, error) {
	panic("not implemented")
}

// fakeAPIKeyStore records IncrementUsage calls only. Everything else in the
// interface must be present to satisfy the compiler but will panic if
// touched, keeping the surface under test small and explicit.
type fakeAPIKeyStore struct {
	calls []incCall
}

type incCall struct {
	id      string
	charged int64
}

func (s *fakeAPIKeyStore) IncrementUsage(_ context.Context, id string, charged int64) error {
	s.calls = append(s.calls, incCall{id: id, charged: charged})
	return nil
}

func (s *fakeAPIKeyStore) Get(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) GetByHash(context.Context, string) (*domain.APIKey, error) {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) Create(context.Context, *domain.APIKey) error {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) Revoke(context.Context, string) error {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) List(context.Context) ([]domain.APIKey, error) {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) ListByCPAPartner(context.Context, string) ([]domain.APIKey, error) {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) Rotate(context.Context, string) (string, error) {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) Pause(context.Context, string) error {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) Resume(context.Context, string) error {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) ResetMonthlyUsage(context.Context, string) error {
	panic("not implemented")
}

func (s *fakeAPIKeyStore) UpdatePlaygroundScope(context.Context, string, domain.PlaygroundScope) error {
	panic("not implemented")
}

// baseJob builds a completed video job with an api key attached so callers
// can drop it into a Recorder without extra setup.
func baseJob() *domain.Job {
	return &domain.Job{
		ID:            "job_1",
		APIKeyID:      "key_a",
		GroupID:       "grp_default",
		AccountID:     "acc_1",
		ModelAlias:    "video-flash",
		JST:           "text2video_seedance",
		UpstreamJobID: "upst_1",
		UpstreamCost:  1000,
		ResultURL:     "https://example/out.mp4",
		Status:        domain.JobCompleted,
		LatencyMS:     4321,
		PollCount:     2,
	}
}

func TestRecorder_UsesBalanceDelta(t *testing.T) {
	events := &fakeEventStore{}
	rec := &Recorder{Events: events}

	job := baseJob()
	acc := &domain.Account{ID: "acc_1", SubscriptionBalance: 700}

	if err := rec.OnJobTerminal(context.Background(), job, acc, 1000, 1.5); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}
	if len(events.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events.events))
	}
	got := events.events[0]
	if got.ActualCreditsHundredths != 300 {
		t.Errorf("actual: got %d want 300 (preBalance 1000 - post 700)", got.ActualCreditsHundredths)
	}
	if got.ChargedCreditsHundredths != 450 {
		t.Errorf("charged: got %d want 450 (300 * 1.5)", got.ChargedCreditsHundredths)
	}
	if got.MarkupPct != 1.5 {
		t.Errorf("markup: got %v want 1.5", got.MarkupPct)
	}
	if got.HiggsgoJobID != "job_1" || got.UpstreamJobID != "upst_1" {
		t.Errorf("job refs not carried through: %+v", got)
	}
	if got.MediaType != "video" {
		t.Errorf("media_type: got %q want video", got.MediaType)
	}
}

func TestRecorder_FallsBackToUpstreamCost(t *testing.T) {
	events := &fakeEventStore{}
	rec := &Recorder{Events: events}

	job := baseJob()
	job.UpstreamCost = 250
	// preBalance=0 → delta path skipped → actual falls back to UpstreamCost.
	if err := rec.OnJobTerminal(context.Background(), job, &domain.Account{ID: "acc_1"}, 0, 1.0); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}
	got := events.events[0]
	if got.ActualCreditsHundredths != 250 {
		t.Errorf("actual fallback: got %d want 250", got.ActualCreditsHundredths)
	}
	if got.ChargedCreditsHundredths != 250 {
		t.Errorf("charged fallback: got %d want 250 (markup 1.0)", got.ChargedCreditsHundredths)
	}

	// Same fallback when the delta would be non-positive (refunded jobs
	// often see post >= pre because higgsfield restored the balance).
	events2 := &fakeEventStore{}
	rec2 := &Recorder{Events: events2}
	if err := rec2.OnJobTerminal(context.Background(), job, &domain.Account{ID: "acc_1", SubscriptionBalance: 5000}, 1000, 1.0); err != nil {
		t.Fatalf("on job terminal (refund): %v", err)
	}
	if events2.events[0].ActualCreditsHundredths != 250 {
		t.Errorf("actual on refund fallback: got %d want 250", events2.events[0].ActualCreditsHundredths)
	}
}

func TestRecorder_MarkupZeroTreatedAsOne(t *testing.T) {
	events := &fakeEventStore{}
	rec := &Recorder{Events: events}

	job := baseJob()
	acc := &domain.Account{ID: "acc_1", SubscriptionBalance: 200}
	// preBalance 500 - post 200 = 300 actual. markup 0 → treated as 1.0.
	if err := rec.OnJobTerminal(context.Background(), job, acc, 500, 0); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}
	got := events.events[0]
	if got.ActualCreditsHundredths != 300 || got.ChargedCreditsHundredths != 300 {
		t.Errorf("markup=0: got actual=%d charged=%d want 300/300", got.ActualCreditsHundredths, got.ChargedCreditsHundredths)
	}
	if got.MarkupPct != 1.0 {
		t.Errorf("markup coerced to: got %v want 1.0", got.MarkupPct)
	}
}

func TestRecorder_IncrementAPIKeyUsage(t *testing.T) {
	events := &fakeEventStore{}
	keys := &fakeAPIKeyStore{}
	rec := &Recorder{Events: events, APIKeys: keys}

	job := baseJob()
	acc := &domain.Account{ID: "acc_1", SubscriptionBalance: 400}
	// preBalance 1000 - post 400 = 600 actual. markup 2.0 → 1200 charged.
	if err := rec.OnJobTerminal(context.Background(), job, acc, 1000, 2.0); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}
	if len(keys.calls) != 1 {
		t.Fatalf("expected 1 IncrementUsage call, got %d", len(keys.calls))
	}
	if keys.calls[0].id != "key_a" || keys.calls[0].charged != 1200 {
		t.Errorf("increment call: got %+v want {id:key_a charged:1200}", keys.calls[0])
	}
}

func TestRecorder_SkipsAPIKeyWhenEmpty(t *testing.T) {
	events := &fakeEventStore{}
	keys := &fakeAPIKeyStore{}
	rec := &Recorder{Events: events, APIKeys: keys}

	job := baseJob()
	job.APIKeyID = "" // CPA mode: no standalone API key row to charge.
	acc := &domain.Account{ID: "acc_1", SubscriptionBalance: 400}

	if err := rec.OnJobTerminal(context.Background(), job, acc, 1000, 1.5); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}
	if len(keys.calls) != 0 {
		t.Errorf("expected 0 IncrementUsage calls, got %d (%+v)", len(keys.calls), keys.calls)
	}
	if len(events.events) != 1 {
		t.Fatalf("event still recorded: got %d rows", len(events.events))
	}
}

func TestRecorder_MediaTypeHeuristic(t *testing.T) {
	cases := []struct {
		jst  string
		want string
	}{
		{"text2video_seedance", "video"},  // matches both "video" and "seedance"
		{"image2video_kling", "video"},    // matches "video" and "kling"
		{"text2image_flux", "image"},      // default branch
		{"text2image_soul", "image"},      // "soul" does not contain "sora"
		{"image2speech_sonilo", "audio"},  // "sonilo" is an audio needle
		{"text2speech_mirelo", "audio"},   // "speech" + "mirelo"
		{"clip_transcriber_pro", "audio"}, // "clip_transcriber"
		{"veo3_generation", "video"},      // "veo3"
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.jst, func(t *testing.T) {
			t.Parallel()
			got := mediaTypeForJST(tc.jst)
			if got != tc.want {
				t.Errorf("mediaTypeForJST(%q) = %q, want %q", tc.jst, got, tc.want)
			}
		})
	}
}

func TestRecorder_NilJobReturnsError(t *testing.T) {
	rec := &Recorder{Events: &fakeEventStore{}}
	err := rec.OnJobTerminal(context.Background(), nil, nil, 0, 0)
	if err == nil {
		t.Fatal("expected error for nil job, got nil")
	}
}

func TestRecorder_NilEventsStore(t *testing.T) {
	rec := &Recorder{Events: nil}
	// A nil Events store must short-circuit even when the rest of the input
	// is nonsensical (nil job, nil account). No panic, no error.
	if err := rec.OnJobTerminal(context.Background(), nil, nil, 0, 0); err != nil {
		t.Errorf("nil events store: got err %v want nil", err)
	}
}

// TestRecorder_InsertErrorPropagates is a small extra guard: if the store
// fails, the caller should see it (metering is best-effort, but tests need
// to be able to assert on the error path).
func TestRecorder_InsertErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	rec := &Recorder{Events: &fakeEventStore{failErr: sentinel}}
	err := rec.OnJobTerminal(context.Background(), baseJob(), &domain.Account{ID: "acc_1"}, 0, 1.0)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}
