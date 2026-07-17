package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMetricsSink is a lock-free stand-in for observability.Metrics used by
// upstream client tests. It records every ObserveUpstreamDuration call so
// tests can assert on labels and rough latency without a real Prometheus
// registry.
type fakeMetricsSink struct {
	mu    sync.Mutex
	calls []metricsCall
}

type metricsCall struct {
	Endpoint string
	Status   string
	Seconds  float64
}

func (f *fakeMetricsSink) ObserveUpstreamDuration(endpoint, status string, seconds float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, metricsCall{Endpoint: endpoint, Status: status, Seconds: seconds})
}

func (f *fakeMetricsSink) snapshot() []metricsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]metricsCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// TestClient_ObservesLatencyOnSuccess: a 200 response with a small artificial
// delay yields exactly one observation carrying the endpoint symbol, status
// "200", and a duration at least as large as the injected sleep.
func TestClient_ObservesLatencyOnSuccess(t *testing.T) {
	const delay = 50 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace_id":"ws_test","credits_balance":100}`))
	}))
	defer srv.Close()

	sink := &fakeMetricsSink{}
	c, _ := newRetryTestClient(t, srv)
	c.Metrics = sink

	if _, err := c.FetchWallet(context.Background(), testAccount()); err != nil {
		t.Fatalf("FetchWallet: %v", err)
	}

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.Endpoint != "fetch_wallet" {
		t.Errorf("endpoint: got %q want fetch_wallet", got.Endpoint)
	}
	if got.Status != "200" {
		t.Errorf("status: got %q want 200", got.Status)
	}
	if got.Seconds < delay.Seconds() {
		t.Errorf("seconds: got %v, want >= %v", got.Seconds, delay.Seconds())
	}
}

// TestClient_ObservesLatencyOnError: a 500 must produce exactly one
// observation with status="500". No retry.
func TestClient_ObservesLatencyOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	sink := &fakeMetricsSink{}
	c, _ := newRetryTestClient(t, srv)
	c.Metrics = sink

	if _, err := c.FetchWallet(context.Background(), testAccount()); err == nil {
		t.Fatalf("expected error on 500, got nil")
	}

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d observations, want 1: %+v", len(calls), calls)
	}
	if calls[0].Status != "500" {
		t.Errorf("status: got %q want 500", calls[0].Status)
	}
	if calls[0].Endpoint != "fetch_wallet" {
		t.Errorf("endpoint: got %q want fetch_wallet", calls[0].Endpoint)
	}
}

// TestClient_ObservesLatencyOnRetry: the first upstream reply is 401 (which
// triggers a JWT remint + one retry), the second is 200. From the SRE's
// perspective this is ONE upstream call that took some total time and
// ultimately succeeded, so the sink must see exactly one observation with
// status="200".
func TestClient_ObservesLatencyOnRetry(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace_id":"ws_test","credits_balance":100}`))
	}))
	defer srv.Close()

	sink := &fakeMetricsSink{}
	c, _ := newRetryTestClient(t, srv)
	c.Metrics = sink

	if _, err := c.FetchWallet(context.Background(), testAccount()); err != nil {
		t.Fatalf("FetchWallet: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits: got %d want 2 (initial 401 + retry)", got)
	}

	calls := sink.snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d observations, want 1 (single terminal event): %+v", len(calls), calls)
	}
	if calls[0].Status != "200" {
		t.Errorf("status: got %q want 200 (terminal status, not intermediate 401)", calls[0].Status)
	}
	if calls[0].Endpoint != "fetch_wallet" {
		t.Errorf("endpoint: got %q want fetch_wallet", calls[0].Endpoint)
	}
}

// TestClient_NoMetricsSinkNoPanic: with Metrics unset (nil sink), the client
// must still complete the request successfully — the metrics wiring is
// strictly optional.
func TestClient_NoMetricsSinkNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace_id":"ws_test","credits_balance":100}`))
	}))
	defer srv.Close()

	c, _ := newRetryTestClient(t, srv)
	if c.Metrics != nil {
		t.Fatalf("precondition: Metrics should be nil by default, got %T", c.Metrics)
	}

	if _, err := c.FetchWallet(context.Background(), testAccount()); err != nil {
		t.Fatalf("FetchWallet with nil Metrics: %v", err)
	}
}
