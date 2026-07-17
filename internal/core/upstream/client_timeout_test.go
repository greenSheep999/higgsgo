package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// newTimeoutTestClient wires an upstream.Client bound to the given httptest
// server with a fake mint client and the caller-supplied per-endpoint
// timeout map. It mirrors newRetryTestClient but lets each test pick its
// own timeouts so we can exercise both the "endpoint present" and "fall
// back to default" branches.
func newTimeoutTestClient(t *testing.T, srv *httptest.Server, timeouts map[string]time.Duration) *Client {
	t.Helper()
	fake := &fakeHTTPClient{mintJWT: newFakeJWT(t, "user_timeout")}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	return New(fake, minter, Config{BaseURL: srv.URL, Timeouts: timeouts})
}

// TestClient_HonorsEndpointTimeout: a 200ms server delay with a 50ms
// per-endpoint timeout must surface a context deadline error. Guards
// against the timeout map being ignored or wired to the wrong endpoint key.
func TestClient_HonorsEndpointTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace_id":"ws","credits_balance":0}`))
	}))
	defer srv.Close()

	c := newTimeoutTestClient(t, srv, map[string]time.Duration{
		"fetch_wallet": 50 * time.Millisecond,
	})

	start := time.Now()
	_, err := c.FetchWallet(context.Background(), testAccount())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil (elapsed=%v)", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got: %v", err)
	}
	// Should give up well before the 200ms server delay completes.
	if elapsed > 150*time.Millisecond {
		t.Errorf("timeout took too long: %v (want < 150ms)", elapsed)
	}
}

// TestClient_DefaultTimeoutOnMissing: an endpoint absent from the map
// falls back to the "default" key. A 200ms server delay with a 50ms
// default must trip the deadline for fetch_user, which is deliberately
// not listed under its own key.
func TestClient_DefaultTimeoutOnMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u"}`))
	}))
	defer srv.Close()

	c := newTimeoutTestClient(t, srv, map[string]time.Duration{
		// fetch_user intentionally omitted.
		"default": 50 * time.Millisecond,
	})

	_, err := c.FetchUser(context.Background(), testAccount())
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded via default fallback, got: %v", err)
	}
}

// TestClient_LongerCreateJobTimeout: a POST payload gets a longer budget
// than a GET status probe. Same server delay, different timeouts — the
// long timeout should succeed while the short one must fail. Locks the
// intended asymmetry between create_job and fetch_status.
func TestClient_LongerCreateJobTimeout(t *testing.T) {
	// Success fixture for create_job: parseCreateResponse needs job_sets[0].jobs[0].
	createBody, err := json.Marshal(map[string]any{
		"id": "js_1",
		"job_sets": []map[string]any{{
			"id":   "js_1",
			"cost": 0,
			"jobs": []map[string]any{{"id": "job_1"}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/jobs/v2/fake":
			_, _ = w.Write(createBody)
		default:
			_, _ = w.Write([]byte(`{"id":"job_1","status":"queued"}`))
		}
	}))
	defer srv.Close()

	c := newTimeoutTestClient(t, srv, map[string]time.Duration{
		"create_job":   500 * time.Millisecond, // roomy — 200ms delay fits
		"fetch_status": 50 * time.Millisecond,  // tight — 200ms trips it
	})

	// Long budget: create_job must succeed.
	resp, err := c.CreateJob(context.Background(), CreateRequest{
		Account:  testAccount(),
		Endpoint: "/jobs/v2/fake",
		Body:     map[string]string{"prompt": "hello"},
	})
	if err != nil {
		t.Fatalf("CreateJob under 500ms budget: %v", err)
	}
	if resp.JobID != "job_1" {
		t.Errorf("JobID: got %q want job_1", resp.JobID)
	}

	// Short budget: fetch_status must trip the deadline.
	_, err = c.FetchStatus(context.Background(), testAccount(), "job_1")
	if err == nil {
		t.Fatalf("FetchStatus expected timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("FetchStatus: expected context.DeadlineExceeded, got: %v", err)
	}
}

// TestClient_NoTimeoutMapAppliesNoDeadline: when Config.Timeouts is left
// empty (as legacy tests do), no context wrapping happens. A slow server
// must be reached to completion regardless of wall time. This is the
// backwards-compat guarantee protecting client_retry_test.go and
// client_metrics_test.go from becoming flaky under this change.
func TestClient_NoTimeoutMapAppliesNoDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"workspace_id":"ws","credits_balance":42}`))
	}))
	defer srv.Close()

	// Explicit nil map — no per-endpoint timeouts configured.
	c := newTimeoutTestClient(t, srv, nil)

	got, err := c.FetchWallet(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchWallet with no timeouts: %v", err)
	}
	if got.CreditsBalance != 42 {
		t.Errorf("CreditsBalance: got %d want 42", got.CreditsBalance)
	}
}

// TestClient_TimeoutForFallback exercises the timeoutFor helper directly
// so future refactors that inline the lookup catch the semantics: exact
// match wins, "default" is used only when the endpoint key is absent,
// and an empty map yields (0, false).
func TestClient_TimeoutForFallback(t *testing.T) {
	cases := []struct {
		name     string
		timeouts map[string]time.Duration
		endpoint string
		wantOK   bool
		want     time.Duration
	}{
		{"empty map", nil, "fetch_wallet", false, 0},
		{"exact match", map[string]time.Duration{"fetch_wallet": 10 * time.Second}, "fetch_wallet", true, 10 * time.Second},
		{"default fallback", map[string]time.Duration{"default": 5 * time.Second}, "fetch_user", true, 5 * time.Second},
		{"exact wins over default", map[string]time.Duration{"fetch_user": 1 * time.Second, "default": 5 * time.Second}, "fetch_user", true, 1 * time.Second},
		{"zero exact drops to default", map[string]time.Duration{"fetch_user": 0, "default": 5 * time.Second}, "fetch_user", true, 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{}
			// Trim zero values the same way New would.
			if len(tc.timeouts) > 0 {
				c.timeouts = make(map[string]time.Duration, len(tc.timeouts))
				for k, v := range tc.timeouts {
					if v > 0 {
						c.timeouts[k] = v
					}
				}
			}
			got, ok := c.timeoutFor(tc.endpoint)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("dur: got %v want %v", got, tc.want)
			}
		})
	}

	// Suppress unused-import warning when the account helper isn't used
	// above; kept because future timeout cases will likely need it.
	_ = domain.Account{}
}
