package regression

// Tests for Ticker: sample selection, image-vs-video filtering, health
// upsert wiring, concurrency bounds, and empty-registry no-op. All fakes
// live in this file so the ticker can be exercised without a real
// proxy.Service or sqlite handle.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeRegistry is a minimal ports.ModelRegistry backed by a static slice.
type fakeRegistry struct {
	specs []*domain.ModelSpec
}

func (r *fakeRegistry) Resolve(alias string) (*domain.ModelSpec, error) {
	for _, s := range r.specs {
		if s.Alias == alias {
			return s, nil
		}
	}
	return nil, domain.ErrModelNotFound
}

func (r *fakeRegistry) List(filter ports.ModelFilter) []*domain.ModelSpec {
	out := make([]*domain.ModelSpec, 0, len(r.specs))
	for _, s := range r.specs {
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

func (r *fakeRegistry) Reload(ctx context.Context) error     { return nil }
func (r *fakeRegistry) ResolveAlias(a string) (string, bool) { return a, true }
func (r *fakeRegistry) StarterLocked(jst string) bool        { return false }

// fakeHealthStore records every Insert and answers Latest from an internal
// map keyed by jst.
type fakeHealthStore struct {
	mu      sync.Mutex
	latest  map[string]ports.ModelHealthRow
	inserts []healthInsert
	// insertErr, when non-nil, is returned by Insert. Tests use this to
	// verify that a failing store does not crash the ticker.
	insertErr error
}

type healthInsert struct {
	JST       string
	Verdict   domain.JobStatus
	CheckedAt time.Time
	Cost      int64
}

func newFakeHealth() *fakeHealthStore {
	return &fakeHealthStore{latest: make(map[string]ports.ModelHealthRow)}
}

func (h *fakeHealthStore) Insert(
	ctx context.Context,
	jst string,
	checkedAt time.Time,
	verdict domain.JobStatus,
	httpStatus int,
	cost int64,
	pollSec int,
) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.insertErr != nil {
		return h.insertErr
	}
	h.inserts = append(h.inserts, healthInsert{
		JST:       jst,
		Verdict:   verdict,
		CheckedAt: checkedAt,
		Cost:      cost,
	})
	h.latest[jst] = ports.ModelHealthRow{
		JST:         jst,
		CheckedAt:   checkedAt,
		Verdict:     verdict,
		HTTPStatus:  httpStatus,
		Cost:        cost,
		PollTimeSec: pollSec,
	}
	return nil
}

func (h *fakeHealthStore) Latest(ctx context.Context, jst string) (*ports.ModelHealthRow, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if row, ok := h.latest[jst]; ok {
		copy := row
		return &copy, nil
	}
	return nil, nil
}

// List satisfies the ports.ModelHealthStore interface. The regression
// ticker never calls List itself; this shim exists so the fake still
// implements the full interface after /admin/model-health added the
// List method.
func (h *fakeHealthStore) List(context.Context) ([]ports.ModelHealthRow, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ports.ModelHealthRow, 0, len(h.latest))
	for _, row := range h.latest {
		out = append(out, row)
	}
	return out, nil
}

func (h *fakeHealthStore) seedLatest(jst string, checkedAt time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest[jst] = ports.ModelHealthRow{JST: jst, CheckedAt: checkedAt, Verdict: domain.JobCompleted}
}

func (h *fakeHealthStore) insertedJSTs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.inserts))
	for _, ins := range h.inserts {
		out = append(out, ins.JST)
	}
	sort.Strings(out)
	return out
}

// fakeProxy records every Generate call and returns a scripted response
// per model alias. When no script exists it returns a completed image with
// a canned result URL.
type fakeProxy struct {
	mu       sync.Mutex
	calls    []string
	scripted map[string]proxyOutcome

	// concurrency instrumentation for TestTicker_ConcurrencyBound.
	inFlight  int32
	maxActive int32
	holdFor   time.Duration
}

type proxyOutcome struct {
	Resp *proxy.GenerationResponse
	Err  error
}

func (p *fakeProxy) Generate(ctx context.Context, req proxy.GenerationRequest) (*proxy.GenerationResponse, error) {
	// Track live concurrency so ConcurrencyBound can verify the semaphore.
	live := atomic.AddInt32(&p.inFlight, 1)
	defer atomic.AddInt32(&p.inFlight, -1)
	for {
		old := atomic.LoadInt32(&p.maxActive)
		if live <= old || atomic.CompareAndSwapInt32(&p.maxActive, old, live) {
			break
		}
	}
	if p.holdFor > 0 {
		select {
		case <-time.After(p.holdFor):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	p.mu.Lock()
	p.calls = append(p.calls, req.Model)
	outcome, ok := p.scripted[req.Model]
	p.mu.Unlock()

	if !ok {
		return &proxy.GenerationResponse{
			ID:        "probe_" + req.Model,
			Object:    "image",
			Model:     req.Model,
			Status:    string(domain.JobCompleted),
			ResultURL: "https://example.com/" + req.Model + ".png",
			Cost:      500,
		}, nil
	}
	return outcome.Resp, outcome.Err
}

func (p *fakeProxy) calledAliases() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	copy(out, p.calls)
	sort.Strings(out)
	return out
}

// specsFixture returns a mixed image/video slice with deterministic JSTs
// so tests can pin exactly which entries the ticker should consume.
func specsFixture() []*domain.ModelSpec {
	return []*domain.ModelSpec{
		// six images
		{Alias: "img-a", JST: "img_a", Output: "image"},
		{Alias: "img-b", JST: "img_b", Output: "image"},
		{Alias: "img-c", JST: "img_c", Output: "image"},
		{Alias: "img-d", JST: "img_d", Output: "image"},
		{Alias: "img-e", JST: "img_e", Output: "image"},
		{Alias: "img-f", JST: "img_f", Output: "image"},
		// four videos — must never be probed
		{Alias: "vid-a", JST: "vid_a", Output: "video"},
		{Alias: "vid-b", JST: "vid_b", Output: "video"},
		{Alias: "vid-c", JST: "vid_c", Output: "video"},
		{Alias: "vid-d", JST: "vid_d", Output: "video"},
	}
}

func TestTicker_ProbesSampleSize(t *testing.T) {
	reg := &fakeRegistry{specs: specsFixture()}
	health := newFakeHealth()
	fake := &fakeProxy{}

	tk := &Ticker{
		Health:      health,
		Registry:    reg,
		Proxy:       fake,
		SampleSize:  3,
		Concurrency: 4,
	}
	tk.applyDefaults()
	tk.tick(context.Background())

	got := fake.calledAliases()
	if len(got) != 3 {
		t.Fatalf("expected 3 probes, got %d (%v)", len(got), got)
	}
	// Every called alias must be an image; no video alias may leak
	// through. The isImageModel guard should also strip any video JST.
	for _, alias := range got {
		if alias[:3] != "img" {
			t.Errorf("unexpected non-image probe alias: %s", alias)
		}
	}

	// Store must have received one Insert per probe.
	inserts := health.insertedJSTs()
	if len(inserts) != 3 {
		t.Errorf("expected 3 health inserts, got %d (%v)", len(inserts), inserts)
	}
}

func TestTicker_UpdatesHealthStore(t *testing.T) {
	// Five image models: three complete, two fail. Health store must see
	// all five inserts with correct verdict split.
	specs := []*domain.ModelSpec{
		{Alias: "ok-1", JST: "ok_1", Output: "image"},
		{Alias: "ok-2", JST: "ok_2", Output: "image"},
		{Alias: "ok-3", JST: "ok_3", Output: "image"},
		{Alias: "fail-1", JST: "fail_1", Output: "image"},
		{Alias: "fail-2", JST: "fail_2", Output: "image"},
	}
	reg := &fakeRegistry{specs: specs}
	health := newFakeHealth()

	fake := &fakeProxy{scripted: map[string]proxyOutcome{
		"ok-1":   {Resp: &proxy.GenerationResponse{Status: string(domain.JobCompleted), ResultURL: "u1", Cost: 500}},
		"ok-2":   {Resp: &proxy.GenerationResponse{Status: string(domain.JobCompleted), ResultURL: "u2", Cost: 500}},
		"ok-3":   {Resp: &proxy.GenerationResponse{Status: string(domain.JobCompleted), ResultURL: "u3", Cost: 500}},
		"fail-1": {Err: errors.New("upstream 500")},
		"fail-2": {Resp: &proxy.GenerationResponse{Status: string(domain.JobFailed)}},
	}}

	tk := &Ticker{
		Health:      health,
		Registry:    reg,
		Proxy:       fake,
		SampleSize:  5,
		Concurrency: 5,
	}
	tk.tick(context.Background())

	// Count verdicts by inspecting the recorded inserts.
	var completed, failed int
	health.mu.Lock()
	for _, ins := range health.inserts {
		switch ins.Verdict {
		case domain.JobCompleted:
			completed++
		case domain.JobFailed:
			failed++
		}
	}
	health.mu.Unlock()

	if completed != 3 {
		t.Errorf("completed count: got %d want 3", completed)
	}
	if failed != 2 {
		t.Errorf("failed count: got %d want 2", failed)
	}
	if len(health.inserts) != 5 {
		t.Errorf("total inserts: got %d want 5", len(health.inserts))
	}
}

func TestTicker_ConcurrencyBound(t *testing.T) {
	// Seven image models, Concurrency=1, each proxy call held for a beat
	// so parallel invocations would show up as maxActive > 1.
	specs := []*domain.ModelSpec{
		{Alias: "c1", JST: "c1", Output: "image"},
		{Alias: "c2", JST: "c2", Output: "image"},
		{Alias: "c3", JST: "c3", Output: "image"},
		{Alias: "c4", JST: "c4", Output: "image"},
		{Alias: "c5", JST: "c5", Output: "image"},
		{Alias: "c6", JST: "c6", Output: "image"},
		{Alias: "c7", JST: "c7", Output: "image"},
	}
	reg := &fakeRegistry{specs: specs}
	fake := &fakeProxy{holdFor: 5 * time.Millisecond}

	tk := &Ticker{
		Health:      newFakeHealth(),
		Registry:    reg,
		Proxy:       fake,
		SampleSize:  7,
		Concurrency: 1,
	}
	tk.tick(context.Background())

	if got := atomic.LoadInt32(&fake.maxActive); got > 1 {
		t.Errorf("maxActive: got %d want <= 1 (concurrency bound violated)", got)
	}
	if len(fake.calls) != 7 {
		t.Errorf("total calls: got %d want 7", len(fake.calls))
	}
}

func TestTicker_SkipsWhenRegistryEmpty(t *testing.T) {
	reg := &fakeRegistry{specs: nil}
	health := newFakeHealth()
	fake := &fakeProxy{}

	tk := &Ticker{
		Health:      health,
		Registry:    reg,
		Proxy:       fake,
		SampleSize:  5,
		Concurrency: 2,
	}
	// No panic, no proxy call, no insert.
	tk.tick(context.Background())
	if len(fake.calls) != 0 {
		t.Errorf("expected 0 proxy calls, got %d", len(fake.calls))
	}
	if len(health.inserts) != 0 {
		t.Errorf("expected 0 health inserts, got %d", len(health.inserts))
	}
}

func TestTicker_SkipUpstream(t *testing.T) {
	// SkipUpstream must record a pending row but never touch the proxy.
	specs := []*domain.ModelSpec{
		{Alias: "img-a", JST: "img_a", Output: "image"},
		{Alias: "img-b", JST: "img_b", Output: "image"},
	}
	reg := &fakeRegistry{specs: specs}
	health := newFakeHealth()
	fake := &fakeProxy{}

	tk := &Ticker{
		Health:       health,
		Registry:     reg,
		Proxy:        fake,
		SampleSize:   2,
		Concurrency:  2,
		SkipUpstream: true,
	}
	tk.tick(context.Background())

	if len(fake.calls) != 0 {
		t.Errorf("SkipUpstream should not call Proxy, got %d calls", len(fake.calls))
	}
	if len(health.inserts) != 2 {
		t.Fatalf("expected 2 pending inserts, got %d", len(health.inserts))
	}
	for _, ins := range health.inserts {
		if ins.Verdict != domain.JobPending {
			t.Errorf("SkipUpstream verdict: got %q want pending", ins.Verdict)
		}
	}
}

func TestTicker_PicksOldestFirst(t *testing.T) {
	// Six image models — two previously probed (recent), one previously
	// probed (very old), three never probed. SampleSize=3 should pick the
	// three never-probed models first (they sort as oldest).
	specs := []*domain.ModelSpec{
		{Alias: "recent-1", JST: "recent_1", Output: "image"},
		{Alias: "recent-2", JST: "recent_2", Output: "image"},
		{Alias: "old-1", JST: "old_1", Output: "image"},
		{Alias: "unseen-1", JST: "unseen_1", Output: "image"},
		{Alias: "unseen-2", JST: "unseen_2", Output: "image"},
		{Alias: "unseen-3", JST: "unseen_3", Output: "image"},
	}
	reg := &fakeRegistry{specs: specs}
	health := newFakeHealth()
	now := time.Now().UTC()
	health.seedLatest("recent_1", now.Add(-1*time.Hour))
	health.seedLatest("recent_2", now.Add(-2*time.Hour))
	health.seedLatest("old_1", now.Add(-30*24*time.Hour))
	fake := &fakeProxy{}

	tk := &Ticker{
		Health:      health,
		Registry:    reg,
		Proxy:       fake,
		SampleSize:  3,
		Concurrency: 3,
	}
	tk.tick(context.Background())

	got := fake.calledAliases()
	want := []string{"unseen-1", "unseen-2", "unseen-3"}
	if len(got) != len(want) {
		t.Fatalf("expected %d probes, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("probe[%d]: got %q want %q", i, got[i], want[i])
		}
	}

	// Follow-up tick with SampleSize=1 should now pick old_1 (the oldest
	// among the previously-probed set) because the unseen models are
	// covered.
	tk.SampleSize = 1
	fake.mu.Lock()
	fake.calls = nil
	fake.mu.Unlock()
	tk.tick(context.Background())
	got = fake.calledAliases()
	if len(got) != 1 || got[0] != "old-1" {
		t.Errorf("second tick: got %v want [old-1]", got)
	}
}
