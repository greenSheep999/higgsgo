package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/webhook"
)

func newWebhooksRouter(d *webhook.Dispatcher) chi.Router {
	r := chi.NewRouter()
	NewWebhooksHandler(d).Register(r)
	return r
}

// TestWebhooksHandler_ReturnsStats verifies the /webhooks/stats endpoint
// returns the Dispatcher's zero snapshot when nothing has been fired,
// serialized under the documented JSON keys.
func TestWebhooksHandler_ReturnsStats(t *testing.T) {
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	d := webhook.New(lg, webhook.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Close immediately — we only exercise the read path.
	defer d.Close(ctx)

	req := httptest.NewRequest(http.MethodGet, "/webhooks/stats", nil)
	w := httptest.NewRecorder()
	newWebhooksRouter(d).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"enqueued", "delivered", "failed", "dropped", "in_flight", "time"} {
		if _, ok := body[key]; !ok {
			t.Errorf("missing key %q in response: %v", key, body)
		}
	}
	// JSON numbers decode to float64.
	for _, key := range []string{"enqueued", "delivered", "failed", "dropped", "in_flight"} {
		v, _ := body[key].(float64)
		if v != 0 {
			t.Errorf("%s = %v, want 0", key, v)
		}
	}
	if _, err := time.Parse(time.RFC3339, body["time"].(string)); err != nil {
		t.Errorf("time not RFC3339: %v", err)
	}
}

// TestWebhooksHandler_NilDispatcher confirms the handler surfaces a
// 503 when wired without a Dispatcher (defensive; server.go currently
// guards against this by only mounting when non-nil).
func TestWebhooksHandler_NilDispatcher(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/webhooks/stats", nil)
	w := httptest.NewRecorder()
	newWebhooksRouter(nil).ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}
