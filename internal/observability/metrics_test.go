package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewMetrics_Constructs(t *testing.T) {
	m := NewMetrics()
	if m == nil {
		t.Fatal("NewMetrics returned nil")
	}
	if m.Registry == nil {
		t.Fatal("Metrics.Registry is nil")
	}
	if m.HTTPRequests == nil || m.HTTPDuration == nil || m.HTTPInFlight == nil {
		t.Fatal("HTTP collectors are nil")
	}
	if m.AccountsActive == nil || m.JobsInFlight == nil {
		t.Fatal("pool gauges are nil")
	}
	if m.UsageCredits == nil {
		t.Fatal("UsageCredits counter is nil")
	}
	// Constructing again in the same process must not panic — every
	// Metrics uses its own private Registry, not the process default.
	_ = NewMetrics()
}

func TestNewMetrics_HandlerExposesMetricNames(t *testing.T) {
	m := NewMetrics()

	// Poke one of every collector so the handler emits at least a
	// zero-valued sample line for it (bare CounterVec/HistogramVec do
	// not appear until WithLabelValues is called on them).
	m.HTTPRequests.WithLabelValues("GET", "/health", "200").Inc()
	m.HTTPDuration.WithLabelValues("GET", "/health").Observe(0.001)
	m.HTTPInFlight.Set(0)
	m.AccountsActive.Set(0)
	m.JobsInFlight.Set(0)
	m.UsageCredits.WithLabelValues("video", "completed").Add(0)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Handler status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	text := string(body)

	wantNames := []string{
		"higgsgo_http_requests_total",
		"higgsgo_http_request_duration_seconds",
		"higgsgo_http_in_flight_requests",
		"higgsgo_accounts_active",
		"higgsgo_jobs_in_flight",
		"higgsgo_usage_credits_hundredths_total",
	}
	for _, name := range wantNames {
		if !strings.Contains(text, name) {
			t.Errorf("metrics body missing %q\n--body--\n%s", name, text)
		}
	}
}
