package observability

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the Prometheus collectors exposed by the /metrics endpoint.
//
// Everything is registered against a private Registry (not the process-wide
// prometheus.DefaultRegisterer) so tests can spin up multiple Metrics
// instances in the same process without a "duplicate metrics collector
// registration attempted" panic and so the default Go/process collectors
// don't leak into scrape output unless we opt in explicitly.
type Metrics struct {
	Registry *prometheus.Registry

	// HTTP.
	//
	// route is the chi RoutePattern (e.g. "/v1/jobs/{id}"), not the raw
	// URL. Raw URLs would explode the time-series cardinality because
	// every job ID would be its own label value.
	HTTPRequests *prometheus.CounterVec   // labels: method, route, status
	HTTPDuration *prometheus.HistogramVec // labels: method, route
	HTTPInFlight prometheus.Gauge

	// Pool state (updated by PoolCollector).
	AccountsActive prometheus.Gauge
	JobsInFlight   prometheus.Gauge

	// Metering.
	//
	// Hooked in from metering.Recorder in a follow-up patch. Left wired
	// here so the collector goroutine and the recorder can share the
	// same Metrics pointer without another refactor.
	UsageCredits *prometheus.CounterVec // labels: media_type, status
}

// NewMetrics builds a Metrics with all collectors registered against a
// fresh private Registry. Never returns nil.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "higgsgo_http_requests_total",
			Help: "Total HTTP requests handled, by method, chi route pattern, and status code.",
		}, []string{"method", "route", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "higgsgo_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, by method and chi route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "higgsgo_http_in_flight_requests",
			Help: "Number of HTTP requests currently being served.",
		}),
		AccountsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "higgsgo_accounts_active",
			Help: "Number of pool accounts currently in status=active.",
		}),
		JobsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "higgsgo_jobs_in_flight",
			Help: "Number of proxied jobs currently in a non-terminal (pending/running) status.",
		}),
		UsageCredits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "higgsgo_usage_credits_hundredths_total",
			Help: "Total credits (in hundredths) charged, partitioned by media_type and terminal job status.",
		}, []string{"media_type", "status"}),
	}
	reg.MustRegister(
		m.HTTPRequests,
		m.HTTPDuration,
		m.HTTPInFlight,
		m.AccountsActive,
		m.JobsInFlight,
		m.UsageCredits,
	)
	return m
}

// Handler returns an http.Handler that serves the Prometheus text-format
// exposition for this Metrics' Registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		Registry: m.Registry,
	})
}
