package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue reads a single sample from a labelled CounterVec cell.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter Write: %v", err)
	}
	if m.Counter == nil || m.Counter.Value == nil {
		return 0
	}
	return *m.Counter.Value
}

// histogramSampleCount reads the sample count from a labelled HistogramVec cell.
func histogramSampleCount(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	h, ok := o.(prometheus.Histogram)
	if !ok {
		t.Fatalf("observer is not a Histogram (got %T)", o)
	}
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatalf("histogram Write: %v", err)
	}
	if m.Histogram == nil || m.Histogram.SampleCount == nil {
		return 0
	}
	return *m.Histogram.SampleCount
}

// newRouter wires a chi router with the HTTPMetrics middleware and one
// route handler. Its shape mirrors what publicRouter does in server.go.
func newRouter(m *observability.Metrics, method, pattern string, handler http.HandlerFunc) http.Handler {
	r := chi.NewRouter()
	r.Use(HTTPMetrics(m, DefaultRoutePattern))
	r.Method(method, pattern, handler)
	return r
}

func TestHTTPMetrics_CountsRequest(t *testing.T) {
	m := observability.NewMetrics()
	router := newRouter(m, http.MethodGet, "/test", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/test", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := counterValue(t, m.HTTPRequests.WithLabelValues("GET", "/test", "200"))
	if got != 1 {
		t.Errorf("HTTPRequests{GET,/test,200} = %v, want 1", got)
	}
}

func TestHTTPMetrics_CountsError(t *testing.T) {
	m := observability.NewMetrics()
	router := newRouter(m, http.MethodPost, "/boom", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "kaboom", http.StatusInternalServerError)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	got := counterValue(t, m.HTTPRequests.WithLabelValues("POST", "/boom", "500"))
	if got != 1 {
		t.Errorf("HTTPRequests{POST,/boom,500} = %v, want 1", got)
	}
}

func TestHTTPMetrics_DurationObserved(t *testing.T) {
	m := observability.NewMetrics()
	router := newRouter(m, http.MethodGet, "/timed", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/timed", nil))

	obs, err := m.HTTPDuration.GetMetricWithLabelValues("GET", "/timed")
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	if got := histogramSampleCount(t, obs); got != 1 {
		t.Errorf("HTTPDuration sample count = %d, want 1", got)
	}
}

func TestHTTPMetrics_UnknownRoute(t *testing.T) {
	m := observability.NewMetrics()
	// No handler registered for /nope — chi answers 404 and RoutePattern
	// returns "" for it. The middleware must bucket that under "unknown".
	router := newRouter(m, http.MethodGet, "/only", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	got := counterValue(t, m.HTTPRequests.WithLabelValues("GET", "unknown", "404"))
	if got != 1 {
		t.Errorf("HTTPRequests{GET,unknown,404} = %v, want 1", got)
	}
}

func TestHTTPMetrics_NilMetricsIsPassthrough(t *testing.T) {
	// A nil Metrics must not blow up — server.go relies on this to
	// keep the wiring optional.
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := HTTPMetrics(nil, DefaultRoutePattern)(next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
}
