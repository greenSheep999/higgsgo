package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// waitFor polls fn until it returns true or the deadline expires. Used to
// synchronize on Dispatcher async delivery without exposing an internal
// drain API. Close(ctx) is the real drain — this helper is only for
// mid-run assertions where we do not want to close the dispatcher yet.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for condition", timeout)
}

func TestDispatcher_StatsInitiallyZero(t *testing.T) {
	d := newTestDispatcher(t, "")
	s := d.Stats()
	if s != (DispatcherStats{}) {
		t.Fatalf("expected zero stats, got %+v", s)
	}
}

func TestDispatcher_StatsIncrementOnFire(t *testing.T) {
	// A server that blocks until we release it, so we can observe
	// InFlight and Enqueued before deliveries drain.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, "")
	for i := 0; i < 3; i++ {
		d.Fire(srv.URL, &domain.Job{ID: "j", ModelAlias: "m", Status: domain.JobCompleted})
	}

	// Enqueued and InFlight should reach 3 (Concurrency is 4 in the
	// test dispatcher, so all three sit in-flight simultaneously).
	waitFor(t, 2*time.Second, func() bool {
		s := d.Stats()
		return s.Enqueued == 3 && s.InFlight == 3
	})

	// Release the server and let everything drain.
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d.Close(ctx)

	s := d.Stats()
	if s.Enqueued != 3 {
		t.Errorf("Enqueued = %d, want 3", s.Enqueued)
	}
	if s.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0", s.InFlight)
	}
}

func TestDispatcher_StatsDeliveredOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, "")
	d.Fire(srv.URL, &domain.Job{ID: "j", ModelAlias: "m", Status: domain.JobCompleted})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d.Close(ctx)

	s := d.Stats()
	if s.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", s.Delivered)
	}
	if s.Failed != 0 {
		t.Errorf("Failed = %d, want 0", s.Failed)
	}
	if s.Enqueued != 1 {
		t.Errorf("Enqueued = %d, want 1", s.Enqueued)
	}
	if s.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0", s.InFlight)
	}
}

func TestDispatcher_StatsFailedOnAllRetriesExhausted(t *testing.T) {
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, "")
	d.Fire(srv.URL, &domain.Job{ID: "j", ModelAlias: "m", Status: domain.JobFailed})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d.Close(ctx)

	s := d.Stats()
	if s.Failed != 1 {
		t.Errorf("Failed = %d, want 1", s.Failed)
	}
	if s.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0", s.Delivered)
	}
	if got := attempts.Load(); got < 2 {
		t.Errorf("attempts = %d, want >= 2 (MaxRetry)", got)
	}
}

func TestDispatcher_StatsDroppedAfterClose(t *testing.T) {
	d := newTestDispatcher(t, "")
	// Close so Fire is rejected.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	d.Close(ctx)

	d.Fire("http://example.invalid", &domain.Job{ID: "j", ModelAlias: "m", Status: domain.JobCompleted})
	d.Fire("http://example.invalid", &domain.Job{ID: "j", ModelAlias: "m", Status: domain.JobCompleted})

	s := d.Stats()
	if s.Dropped != 2 {
		t.Errorf("Dropped = %d, want 2", s.Dropped)
	}
	if s.Enqueued != 0 {
		t.Errorf("Enqueued = %d, want 0", s.Enqueued)
	}
}
