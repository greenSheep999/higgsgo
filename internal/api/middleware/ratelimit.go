package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Default rate-limit parameters applied when zero-values are passed in.
const (
	defaultRateLimitRPS   = 5.0
	defaultRateLimitBurst = 10
	// bucketIdleEviction is how long a per-key bucket may sit unused before
	// the next Middleware call opportunistically drops it. Keeps memory
	// bounded when a large population of one-shot keys hits the server.
	bucketIdleEviction = time.Hour
)

// bucket is a per-API-key token bucket. It is intentionally minimal —
// no third-party dep is pulled in for this since golang.org/x/time/rate is
// not in go.mod.
type bucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

// RateLimit is a per-API-key token-bucket HTTP middleware.
//
// Requests are keyed off the APIKey stashed in the request context by
// APIKeyAuth. Requests with no APIKey in context (e.g. the optional-auth
// /v1/models discovery routes) are passed through unlimited; the auth
// middleware itself gates those.
type RateLimit struct {
	// RPS is the sustained refill rate, in requests per second per API key.
	// Values <= 0 are replaced with defaultRateLimitRPS.
	RPS float64
	// Burst is the maximum tokens the bucket can hold. Values <= 0 are
	// replaced with defaultRateLimitBurst.
	Burst int
	// Logger optionally records 429 events. Nil disables logging.
	Logger *slog.Logger

	mu      sync.Mutex
	buckets map[string]*bucket
}

// Middleware returns the http.Handler wrapper. Safe to call once and reuse.
func (rl *RateLimit) Middleware(next http.Handler) http.Handler {
	rps, burst := rl.effectiveParams()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k, ok := APIKeyFromContext(r.Context())
		if !ok || k == nil || k.ID == "" {
			// No key => nothing to key the bucket on. Let auth middleware
			// deal with rejecting or allowing the request.
			next.ServeHTTP(w, r)
			return
		}
		if rl.allow(k.ID, rps, burst) {
			next.ServeHTTP(w, r)
			return
		}
		if rl.Logger != nil {
			rl.Logger.Warn("rate limited",
				slog.String("api_key_id", k.ID),
				slog.Float64("rps", rps),
				slog.Int("burst", burst),
				slog.String("path", r.URL.Path),
			)
		}
		writeRateLimited(w)
	})
}

// effectiveParams substitutes defaults for zero/negative RPS/Burst.
func (rl *RateLimit) effectiveParams() (float64, int) {
	rps := rl.RPS
	if rps <= 0 {
		rps = defaultRateLimitRPS
	}
	burst := rl.Burst
	if burst <= 0 {
		burst = defaultRateLimitBurst
	}
	return rps, burst
}

// allow refills the bucket for apiKeyID and consumes one token if available.
// Returns true when the request may proceed. Also performs opportunistic
// eviction of stale buckets (last_seen older than bucketIdleEviction).
func (rl *RateLimit) allow(apiKeyID string, rps float64, burst int) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.buckets == nil {
		rl.buckets = make(map[string]*bucket)
	}

	// Lazy eviction: drop any bucket idle longer than bucketIdleEviction.
	// Cheap: happens under the lock we already hold and only scans the map
	// once per request, which is bounded by the caller's request rate.
	for id, b := range rl.buckets {
		if id == apiKeyID {
			continue
		}
		if now.Sub(b.lastSeen) > bucketIdleEviction {
			delete(rl.buckets, id)
		}
	}

	b, ok := rl.buckets[apiKeyID]
	if !ok {
		// First request: bucket starts full so a fresh key gets the full
		// burst allowance right away.
		b = &bucket{tokens: float64(burst), last: now, lastSeen: now}
		rl.buckets[apiKeyID] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * rps
			if b.tokens > float64(burst) {
				b.tokens = float64(burst)
			}
			b.last = now
		}
		b.lastSeen = now
	}

	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}

// writeRateLimited emits the JSON body and headers for a 429 response.
func writeRateLimited(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(1))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "rate_limited",
			"message": "too many requests for this API key; retry shortly",
		},
	})
}
