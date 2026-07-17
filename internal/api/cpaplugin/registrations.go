package cpaplugin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// HandleRegistrations is a stub for the async-registration status endpoint
// described in docs/POOL-AND-CPA.md §4.2. The current /internal/register
// route is synchronous (it mints an api key and returns immediately) so
// this route has nothing to report yet — it returns 501 with a stable
// error shape so upstream callers can code against the response today.
//
// When RegistrationStore lands in ports and is wired into a background
// registration worker, this handler should look the row up and return
// { registration_id, status, account_id?, error? }.
func (h *Handler) HandleRegistrations(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"registration_id": id,
		"error": map[string]any{
			"type":    "not_implemented",
			"message": "async registration TODO",
		},
	})
}

// HandleStatus is a lightweight partner-scoped health probe. Returns a
// snapshot of the pool sizing so the CPA platform can distinguish
// "higgsgo is up but the pool is empty" (which would cause every
// /internal/execute to fail with no accounts available) from a real
// outage. The parent api package already mounts /health at the router
// root; this handler lives under /internal/status to avoid clobbering it.
func (h *Handler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"mode":            "cpa_plugin",
		"accounts_active": 0,
		"keys_active":     0,
	}
	if h.Accounts != nil {
		// Empty filter -> every row; we can afford this at status-probe
		// cadence.
		accs, err := h.Accounts.List(r.Context(), ports.AccountFilter{})
		if err == nil {
			var active int
			for i := range accs {
				if accs[i].Status == domain.StatusActive {
					active++
				}
			}
			resp["accounts_active"] = active
			resp["accounts_total"] = len(accs)
		}
	}
	if h.APIKeys != nil {
		keys, err := h.APIKeys.List(r.Context())
		if err == nil {
			var active int
			for i := range keys {
				if keys[i].Status == "active" {
					active++
				}
			}
			resp["keys_active"] = active
			resp["keys_total"] = len(keys)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
