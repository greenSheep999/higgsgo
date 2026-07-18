package middleware

// Tests for PlaygroundGate: the /v1/playground/* pre-flight middleware
// that rejects keys with scope=none and passes through cheap/full.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// serveWithKey runs r through PlaygroundGate with the given APIKey in
// context (bypassing APIKeyAuth) and returns the recorder plus whether
// the downstream handler was reached.
func serveWithKey(t *testing.T, key *domain.APIKey) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/playground/models", nil)
	if key != nil {
		req = req.WithContext(ContextWithAPIKey(context.Background(), key))
	}
	rec := httptest.NewRecorder()
	PlaygroundGate()(next).ServeHTTP(rec, req)
	return rec, reached
}

func TestPlaygroundGate_NoneRejected(t *testing.T) {
	rec, reached := serveWithKey(t, &domain.APIKey{
		ID:              "key_none",
		Status:          domain.APIKeyStatusActive,
		PlaygroundScope: domain.PlaygroundScopeNone,
	})
	if reached {
		t.Fatalf("downstream handler reached for scope=none")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rec.Body.String())
	}
	errObj, _ := body["error"].(map[string]any)
	if got, _ := errObj["type"].(string); got != "playground_disabled" {
		t.Errorf("error type: got %q want playground_disabled", got)
	}
}

func TestPlaygroundGate_EmptyScopeRejected(t *testing.T) {
	// An empty PlaygroundScope column must behave the same as "none" — the
	// middleware fails closed so a legacy row cannot accidentally reach the
	// playground handlers.
	_, reached := serveWithKey(t, &domain.APIKey{
		ID:     "key_empty",
		Status: domain.APIKeyStatusActive,
	})
	if reached {
		t.Fatalf("downstream reached for empty scope")
	}
}

func TestPlaygroundGate_MissingKeyRejected(t *testing.T) {
	// No APIKey in context: fail closed. Reaching this branch in production
	// means a wiring bug (PlaygroundGate mounted without APIKeyAuth), so
	// deny is the safe default.
	rec, reached := serveWithKey(t, nil)
	if reached {
		t.Fatalf("downstream reached without an api key in context")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
}

func TestPlaygroundGate_CheapAllowed(t *testing.T) {
	rec, reached := serveWithKey(t, &domain.APIKey{
		ID:              "key_cheap",
		Status:          domain.APIKeyStatusActive,
		PlaygroundScope: domain.PlaygroundScopeCheap,
	})
	if !reached {
		t.Fatalf("downstream not reached for scope=cheap (status=%d, body=%q)",
			rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
}

func TestPlaygroundGate_FullAllowed(t *testing.T) {
	rec, reached := serveWithKey(t, &domain.APIKey{
		ID:              "key_full",
		Status:          domain.APIKeyStatusActive,
		PlaygroundScope: domain.PlaygroundScopeFull,
	})
	if !reached {
		t.Fatalf("downstream not reached for scope=full (status=%d, body=%q)",
			rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
}

// TestPlaygroundGate_AdminBearerAllowed covers the WebUI admin-login flow:
// a request whose context carries the admin-bearer marker (set by
// BearerAuth or PlaygroundAuth on a matching deploy secret) skips the
// scope check entirely and reaches the downstream handler, where the
// per-model resolver treats the caller as scope=full.
func TestPlaygroundGate_AdminBearerAllowed(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/playground/models", nil)
	req = req.WithContext(WithAdminBearer(context.Background()))
	rec := httptest.NewRecorder()
	PlaygroundGate()(next).ServeHTTP(rec, req)
	if !reached {
		t.Fatalf("downstream not reached for admin bearer (status=%d, body=%q)",
			rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
}
