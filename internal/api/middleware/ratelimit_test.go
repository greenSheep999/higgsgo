package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// newRateLimitRequest returns an httptest.NewRequest with the given apiKeyID
// planted in context under the middleware's unexported ctx key. Empty
// apiKeyID means "no APIKey in context" and must trigger passthrough.
func newRateLimitRequest(apiKeyID string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", nil)
	if apiKeyID == "" {
		return r
	}
	k := &domain.APIKey{ID: apiKeyID, Status: "active"}
	ctx := context.WithValue(r.Context(), apiKeyCtxKey{}, k)
	return r.WithContext(ctx)
}

// countingHandler is the dummy `next` handler. Increments Calls each time it
// is invoked.
type countingHandler struct {
	Calls atomic.Int64
}

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.Calls.Add(1)
	w.WriteHeader(http.StatusOK)
}

func TestRateLimit_AllowsUnderBurst(t *testing.T) {
	rl := &RateLimit{RPS: 1, Burst: 5}
	h := &countingHandler{}
	handler := rl.Middleware(h)

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newRateLimitRequest("key_under"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: got status %d, want 200", i+1, w.Code)
		}
	}
	if got := h.Calls.Load(); got != 5 {
		t.Fatalf("next handler invoked %d times, want 5", got)
	}
}

func TestRateLimit_Denies429AboveBurst(t *testing.T) {
	rl := &RateLimit{RPS: 1, Burst: 5}
	h := &countingHandler{}
	handler := rl.Middleware(h)

	// First 5 succeed.
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newRateLimitRequest("key_over"))
		if w.Code != http.StatusOK {
			t.Fatalf("warmup request %d: got status %d, want 200", i+1, w.Code)
		}
	}
	// 6th blows the burst.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newRateLimitRequest("key_over"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("burst+1 request: got status %d, want 429", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Fatalf("Retry-After header missing on 429")
	}
	body := w.Body.String()
	if !strings.Contains(body, "rate_limited") {
		t.Fatalf("429 body missing 'rate_limited', got: %s", body)
	}
	// Sanity: response is well-formed JSON with the expected shape.
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("429 body not JSON: %v (body=%s)", err, body)
	}
	if parsed.Error.Type != "rate_limited" {
		t.Fatalf("429 body error.type=%q, want rate_limited", parsed.Error.Type)
	}
}

func TestRateLimit_Refills(t *testing.T) {
	rl := &RateLimit{RPS: 1, Burst: 2}
	h := &countingHandler{}
	handler := rl.Middleware(h)

	// Drain the burst.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newRateLimitRequest("key_refill"))
		if w.Code != http.StatusOK {
			t.Fatalf("drain request %d: got %d, want 200", i+1, w.Code)
		}
	}
	// Immediate next request should 429.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newRateLimitRequest("key_refill"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 immediately after drain, got %d", w.Code)
	}
	// After 1.1s at RPS=1 we have ~1.1 refilled tokens: next request passes.
	time.Sleep(1100 * time.Millisecond)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, newRateLimitRequest("key_refill"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after refill sleep, got %d", w.Code)
	}
}

func TestRateLimit_PerKeyIsolation(t *testing.T) {
	rl := &RateLimit{RPS: 1, Burst: 2}
	h := &countingHandler{}
	handler := rl.Middleware(h)

	// key_a drains its burst.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newRateLimitRequest("key_a"))
		if w.Code != http.StatusOK {
			t.Fatalf("key_a warmup %d: got %d, want 200", i+1, w.Code)
		}
	}
	// key_a is now over budget.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newRateLimitRequest("key_a"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("key_a expected 429, got %d", w.Code)
	}
	// key_b still has its full burst.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, newRateLimitRequest("key_b"))
	if w.Code != http.StatusOK {
		t.Fatalf("key_b expected 200 (isolated), got %d", w.Code)
	}
}

func TestRateLimit_NoAPIKeyPassthrough(t *testing.T) {
	// Even with RPS/Burst=1 we should never 429 an anonymous request.
	rl := &RateLimit{RPS: 1, Burst: 1}
	h := &countingHandler{}
	handler := rl.Middleware(h)

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newRateLimitRequest(""))
		if w.Code != http.StatusOK {
			t.Fatalf("passthrough request %d: got %d, want 200", i+1, w.Code)
		}
	}
	if got := h.Calls.Load(); got != 10 {
		t.Fatalf("next handler called %d times, want 10", got)
	}
}

func TestRateLimit_ConcurrentSafe(t *testing.T) {
	const burst = 20
	rl := &RateLimit{RPS: 0.01, Burst: burst} // refill effectively negligible during test
	h := &countingHandler{}
	handler := rl.Middleware(h)

	var wg sync.WaitGroup
	var ok200, ok429 atomic.Int64
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, newRateLimitRequest("key_concurrent"))
			switch w.Code {
			case http.StatusOK:
				ok200.Add(1)
			case http.StatusTooManyRequests:
				ok429.Add(1)
			}
		}()
	}
	wg.Wait()

	if ok200.Load() != int64(burst) {
		t.Fatalf("concurrent: got %d successes, want %d", ok200.Load(), burst)
	}
	if ok429.Load() != 0 {
		t.Fatalf("concurrent: got %d 429s, want 0 (all should fit in burst)", ok429.Load())
	}

	// Now push one over: expect 429.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newRateLimitRequest("key_concurrent"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("post-drain request: got %d, want 429", w.Code)
	}
	if got := h.Calls.Load(); got != int64(burst) {
		t.Fatalf("next handler invoked %d times, want %d", got, burst)
	}
}
