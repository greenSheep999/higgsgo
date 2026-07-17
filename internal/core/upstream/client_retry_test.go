package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// countingMintClient satisfies ports.UpstreamClient. It short-circuits calls
// to clerk.higgsfield.ai with a canned JWT-mint reply whose "jwt" value is
// distinct on each call ("jwt-1", "jwt-2", ...), and forwards other requests
// to Go's default HTTP transport. The counter lets a test assert how many
// times a fresh mint was issued.
type countingMintClient struct {
	mintCalls atomic.Int64
	newJWT    func(seq int64) string
}

func (f *countingMintClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		n := f.mintCalls.Add(1)
		body := fmt.Sprintf(`{"jwt":%q}`, f.newJWT(n))
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return http.DefaultClient.Do(req.WithContext(ctx))
}

func (f *countingMintClient) Fingerprint() string { return "counting-fake" }
func (f *countingMintClient) Name() string        { return "counting-fake" }

// newFakeJWTWithSub builds a valid 3-segment JWT with a sub claim of the
// caller's choice. Signature bytes are irrelevant; the upstream package
// never verifies them.
func newFakeJWTWithSub(t *testing.T, sub string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims := map[string]any{
		"sub":   sub,
		"email": "test@example.com",
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
	}
	claimBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimBytes)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + payload + "." + sig
}

// newRetryTestClient wires an upstream.Client with a counting mint client
// that hands out a distinct JWT on every mint call.
func newRetryTestClient(t *testing.T, srv *httptest.Server) (*Client, *countingMintClient) {
	t.Helper()
	fake := &countingMintClient{
		newJWT: func(seq int64) string {
			return newFakeJWTWithSub(t, fmt.Sprintf("user_retry_%d", seq))
		},
	}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	return New(fake, minter, Config{BaseURL: srv.URL}), fake
}

// TestClient_RetryOn401 covers the happy remint path: the first upstream
// call returns 401, the client invalidates the cached JWT, mints a new one,
// and the second call succeeds. Verifies both requests actually reached the
// upstream server and that they carried different Authorization headers.
func TestClient_RetryOn401(t *testing.T) {
	var hits atomic.Int64
	var auth1, auth2 string

	fixture := `{
		"id": "user_abc",
		"email": "test@example.com",
		"plan_type": "plus"
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		switch n {
		case 1:
			auth1 = r.Header.Get("Authorization")
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		case 2:
			auth2 = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fixture))
		default:
			t.Errorf("unexpected extra hit: %d", n)
			http.Error(w, "too many", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c, _ := newRetryTestClient(t, srv)
	got, err := c.FetchUser(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchUser: %v", err)
	}
	if got.ID != "user_abc" {
		t.Errorf("ID: got %q want user_abc", got.ID)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream hits: got %d want 2", n)
	}
	if auth1 == "" || auth2 == "" {
		t.Fatalf("missing Authorization on one of the calls: %q / %q", auth1, auth2)
	}
	if auth1 == auth2 {
		t.Errorf("Authorization header did not change after remint: both %q", auth1)
	}
	if !strings.HasPrefix(auth1, "Bearer ") || !strings.HasPrefix(auth2, "Bearer ") {
		t.Errorf("expected Bearer prefix on both: %q / %q", auth1, auth2)
	}
}

// TestClient_DoubleFailure covers the "still bad after remint" path: both
// upstream attempts return 401. The client must NOT loop; it retries once
// and surfaces ErrUpstreamUnauthorized.
func TestClient_DoubleFailure(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c, _ := newRetryTestClient(t, srv)
	_, err := c.FetchUser(context.Background(), testAccount())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, domain.ErrUpstreamUnauthorized) {
		t.Errorf("expected ErrUpstreamUnauthorized, got: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream hits: got %d want 2 (initial + one retry)", n)
	}
}

// TestClient_500NotRetried ensures non-401 errors are not retried. A 500
// must surface immediately after a single upstream hit.
func TestClient_500NotRetried(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, _ := newRetryTestClient(t, srv)
	_, err := c.FetchUser(context.Background(), testAccount())
	if err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
	if errors.Is(err, domain.ErrUpstreamUnauthorized) {
		t.Errorf("500 must not surface as unauthorized: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hits: got %d want 1 (no retry on 500)", n)
	}
}

// TestClient_400NotRetried ensures a 400 is not retried either — only 401
// triggers the remint path.
func TestClient_400NotRetried(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	c, _ := newRetryTestClient(t, srv)
	_, err := c.FetchUser(context.Background(), testAccount())
	if err == nil {
		t.Fatalf("expected error on 400, got nil")
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hits: got %d want 1 (no retry on 400)", n)
	}
}

// TestClient_SuccessNoRetry covers the plain-success path: no retry, no
// second mint, one upstream hit.
func TestClient_SuccessNoRetry(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u","plan_type":"free"}`))
	}))
	defer srv.Close()

	c, mint := newRetryTestClient(t, srv)
	got, err := c.FetchUser(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchUser: %v", err)
	}
	if got.ID != "u" {
		t.Errorf("ID: got %q want u", got.ID)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("upstream hits: got %d want 1", n)
	}
	if n := mint.mintCalls.Load(); n != 1 {
		t.Errorf("mint calls: got %d want 1 (no invalidation should have occurred)", n)
	}
}
