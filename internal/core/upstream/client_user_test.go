package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeHTTPClient satisfies ports.UpstreamClient. It short-circuits calls to
// clerk.higgsfield.ai with a canned JWT-mint reply, and forwards everything
// else to Go's default HTTP transport (used against a local httptest.Server).
type fakeHTTPClient struct {
	// mintJWT is returned as the "jwt" field of the clerk mint response.
	mintJWT string
}

func (f *fakeHTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		body := fmt.Sprintf(`{"jwt":%q}`, f.mintJWT)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return http.DefaultClient.Do(req.WithContext(ctx))
}

func (f *fakeHTTPClient) Fingerprint() string { return "fake" }
func (f *fakeHTTPClient) Name() string        { return "fake" }

// newFakeJWT builds a syntactically valid 3-segment JWT whose payload
// carries an exp claim well in the future. Signature is irrelevant here —
// the upstream package never verifies it.
func newFakeJWT(t *testing.T, sub string) string {
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

// testAccount returns an account minimally populated for upstream calls
// (SessionID + CookiesJSON are required by jwt.Minter.mint).
func testAccount() *domain.Account {
	return &domain.Account{
		ID:               "user_test_1",
		Email:            "test@example.com",
		SessionID:        "sess_test",
		CookiesJSON:      `{"__session":"stub"}`,
		UserAgent:        "Mozilla/5.0 (test)",
		DataDomeClientID: "dd_test",
	}
}

// newTestClient wires up an upstream.Client that talks to the given
// httptest.Server and mints fake JWTs.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	fake := &fakeHTTPClient{mintJWT: newFakeJWT(t, "user_test_1")}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	return New(fake, minter, Config{BaseURL: srv.URL})
}

func TestClient_FetchUser_Success(t *testing.T) {
	fixture := `{
		"id": "user_abc",
		"email": "test@example.com",
		"plan_type": "plus",
		"subscription_credits": 12.5,
		"package_credits": 4.25,
		"daily_credits": 0,
		"total_plan_credits": 100.0,
		"billing_period": "monthly",
		"plan_ends_at": "2026-08-17T10:00:00Z",
		"has_unlim": true,
		"has_flex_unlim": true,
		"is_pro_plan_veo3_available": true,
		"cohort": "cohort_a",
		"workspace_id": "ws_abc"
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.FetchUser(context.Background(), testAccount())
	if err != nil {
		t.Fatalf("FetchUser: %v", err)
	}

	// Verify every field the refresher relies on.
	if got.ID != "user_abc" {
		t.Errorf("ID: got %q want user_abc", got.ID)
	}
	if got.PlanType != "plus" {
		t.Errorf("PlanType: got %q want plus", got.PlanType)
	}
	if !got.HasUnlim {
		t.Errorf("HasUnlim: got false want true")
	}
	if !got.HasFlexUnlim {
		t.Errorf("HasFlexUnlim: got false want true")
	}
	if !got.IsProVeo3Available {
		t.Errorf("IsProVeo3Available: got false want true")
	}
	if got.Cohort != "cohort_a" {
		t.Errorf("Cohort: got %q want cohort_a", got.Cohort)
	}
	if got.SubscriptionCredits != 12.5 {
		t.Errorf("SubscriptionCredits: got %v want 12.5", got.SubscriptionCredits)
	}
	if got.TotalPlanCredits != 100.0 {
		t.Errorf("TotalPlanCredits: got %v want 100.0", got.TotalPlanCredits)
	}
	if got.PlanEndsAt != "2026-08-17T10:00:00Z" {
		t.Errorf("PlanEndsAt: got %q want 2026-08-17T10:00:00Z", got.PlanEndsAt)
	}
	if got.WorkspaceID != "ws_abc" {
		t.Errorf("WorkspaceID: got %q want ws_abc", got.WorkspaceID)
	}
}

func TestClient_FetchUser_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchUser(context.Background(), testAccount())
	if err == nil {
		t.Fatalf("expected error on 500, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status 500: %v", err)
	}
}

func TestClient_FetchUser_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json at all"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.FetchUser(context.Background(), testAccount())
	if err == nil {
		t.Fatalf("expected JSON parse error, got nil")
	}
}

func TestClient_FetchUser_SendsJWTHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u","plan_type":"free"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if _, err := c.FetchUser(context.Background(), testAccount()); err != nil {
		t.Fatalf("FetchUser: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization header missing Bearer prefix: %q", gotAuth)
	}
	if len(gotAuth) <= len("Bearer ") {
		t.Errorf("Authorization header empty after Bearer prefix: %q", gotAuth)
	}
}
