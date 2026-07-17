package metering

// Tests for the Prometheus UsageCredits counter wired up in
// Recorder.OnJobTerminal. These tests use a real observability.Metrics
// (Prometheus counters are cheap and their values are readable directly
// via the dto interface, so a mock is not worth the maintenance cost).
//
// Label order under test is (media_type, status). Any drift in
// observability/metrics.go would show up here as a "counter not found"
// or wrong-bucket failure — the point of exercising the counter through
// the recorder rather than by hand.

import (
	"context"
	"errors"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/observability"
)

// readCounter returns the current float64 value stored in the counter
// child identified by (mediaType, status). It never registers a new
// child: WithLabelValues on a counter that has not been written to
// yields a zero-valued child, which is exactly what we want when we
// assert "no increment happened".
func readCounter(t *testing.T, m *observability.Metrics, mediaType, status string) float64 {
	t.Helper()
	c := m.UsageCredits.WithLabelValues(mediaType, status)
	var pb dto.Metric
	if err := c.Write(&pb); err != nil {
		t.Fatalf("counter write to dto: %v", err)
	}
	return pb.GetCounter().GetValue()
}

func TestRecorder_IncrementsUsageCreditsMetric(t *testing.T) {
	metrics := observability.NewMetrics()
	events := &fakeEventStore{}
	rec := &Recorder{Events: events, Metrics: metrics}

	job := baseJob() // video / seedance JST, JobCompleted status.
	acc := &domain.Account{ID: "acc_1", SubscriptionBalance: 400}
	// preBalance 1000 - post 400 = 600 actual, markup 1.5 -> 900 charged.
	if err := rec.OnJobTerminal(context.Background(), job, acc, 1000, 1.5); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}

	if got := readCounter(t, metrics, "video", "completed"); got != 900 {
		t.Errorf("usage counter (video,completed): got %v want 900", got)
	}
	// Nothing should have leaked into unrelated label combinations.
	if got := readCounter(t, metrics, "image", "completed"); got != 0 {
		t.Errorf("usage counter (image,completed): got %v want 0", got)
	}
	if got := readCounter(t, metrics, "video", "failed"); got != 0 {
		t.Errorf("usage counter (video,failed): got %v want 0", got)
	}
}

func TestRecorder_MetricsNilNoIncrement(t *testing.T) {
	// The critical property here is "no panic when Metrics is nil".
	// Behavior on the events store must otherwise match the baseline
	// tests in metering_test.go.
	events := &fakeEventStore{}
	rec := &Recorder{Events: events, Metrics: nil}

	job := baseJob()
	acc := &domain.Account{ID: "acc_1", SubscriptionBalance: 400}
	if err := rec.OnJobTerminal(context.Background(), job, acc, 1000, 1.5); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}
	if len(events.events) != 1 {
		t.Fatalf("expected 1 event stored, got %d", len(events.events))
	}
}

func TestRecorder_NoMetricOnInsertFailure(t *testing.T) {
	// When the store rejects the insert, the counter must not move —
	// otherwise the emitted /metrics stream would over-report charged
	// credits vs. the usage_events table used for actual billing.
	metrics := observability.NewMetrics()
	sentinel := errors.New("boom")
	rec := &Recorder{
		Events:  &fakeEventStore{failErr: sentinel},
		Metrics: metrics,
	}

	job := baseJob()
	acc := &domain.Account{ID: "acc_1", SubscriptionBalance: 400}
	err := rec.OnJobTerminal(context.Background(), job, acc, 1000, 1.5)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}

	if got := readCounter(t, metrics, "video", "completed"); got != 0 {
		t.Errorf("counter moved despite insert failure: got %v want 0", got)
	}
}

func TestRecorder_NoMetricOnZeroCharged(t *testing.T) {
	// A refunded job with zero upstream cost and zero balance delta
	// produces chargedCreditsH=0. We deliberately skip the counter in
	// that case to avoid materializing a label combination with an
	// eternally-zero value in the /metrics output.
	metrics := observability.NewMetrics()
	events := &fakeEventStore{}
	rec := &Recorder{Events: events, Metrics: metrics}

	job := baseJob()
	job.UpstreamCost = 0
	job.Status = domain.JobRefunded
	// preBalance=0 -> delta path skipped; UpstreamCost=0 -> actual=0 ->
	// chargedCreditsH=0 regardless of markup.
	if err := rec.OnJobTerminal(context.Background(), job, &domain.Account{ID: "acc_1"}, 0, 1.0); err != nil {
		t.Fatalf("on job terminal: %v", err)
	}
	if len(events.events) != 1 {
		t.Fatalf("expected 1 event stored, got %d", len(events.events))
	}
	if got := readCounter(t, metrics, "video", "refunded"); got != 0 {
		t.Errorf("counter incremented despite zero charged: got %v want 0", got)
	}
}
