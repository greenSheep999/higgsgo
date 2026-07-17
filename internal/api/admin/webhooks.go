package admin

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/webhook"
)

// WebhooksHandler exposes read-only Dispatcher stats to operators.
//
// The Dispatcher field is a concrete *webhook.Dispatcher rather than an
// interface. admin already depends on several core packages
// (apikey, cpaplugin via server.go), so an extra concrete dep here does
// not violate the dependency direction. Using the concrete type also
// keeps wiring in server.go trivial.
type WebhooksHandler struct {
	Dispatcher *webhook.Dispatcher
}

// NewWebhooksHandler builds a handler over the given dispatcher.
func NewWebhooksHandler(d *webhook.Dispatcher) *WebhooksHandler {
	return &WebhooksHandler{Dispatcher: d}
}

// Register mounts the routes under /admin/webhooks.
func (h *WebhooksHandler) Register(r chi.Router) {
	r.Get("/webhooks/stats", h.Stats)
}

// Stats returns a JSON snapshot of the Dispatcher counters.
func (h *WebhooksHandler) Stats(w http.ResponseWriter, _ *http.Request) {
	if h.Dispatcher == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "webhook dispatcher not configured")
		return
	}
	s := h.Dispatcher.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"enqueued":  s.Enqueued,
		"delivered": s.Delivered,
		"failed":    s.Failed,
		"dropped":   s.Dropped,
		"in_flight": s.InFlight,
		"time":      time.Now().UTC().Format(time.RFC3339),
	})
}
