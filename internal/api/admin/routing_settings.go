package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// SettingKeyRoutingStrategyDefault is the system_settings row key used
// to persist the operator's global default routing preference. The
// value is one of the two sidebar radio options ("load_balance" or
// "priority"); the mapping to a concrete domain.RouteStrategy happens
// in ResolveDefaultRouteStrategy so the front-end vocabulary stays
// small and future policy changes (e.g. swapping load_balance's
// underlying strategy from round_robin to least_used) do not require a
// UI change.
const SettingKeyRoutingStrategyDefault = "routing_strategy_default"

// RoutingPreference is the front-end vocabulary for the sidebar
// "Load balance / Priority" radio. Kept as a separate type from
// domain.RouteStrategy so a caller cannot accidentally pass a group-
// level strategy value (e.g. "least_used") where a preference is
// expected.
type RoutingPreference string

const (
	// RoutingPreferenceLoadBalance spreads jobs across the pool. The
	// concrete strategy applied to a new group is
	// domain.RouteRoundRobin — see ResolveDefaultRouteStrategy.
	RoutingPreferenceLoadBalance RoutingPreference = "load_balance"

	// RoutingPreferencePriority prefers higher-priority accounts. Maps
	// to domain.RoutePriority when creating a new group.
	RoutingPreferencePriority RoutingPreference = "priority"

	// DefaultRoutingPreference is the fallback used when neither the
	// system_settings row nor an explicit request body value is
	// present. Keep this in sync with the sidebar's default checked
	// radio so the "no override" UX matches what new groups will get.
	DefaultRoutingPreference = RoutingPreferenceLoadBalance
)

// IsValidRoutingPreference reports whether s is one of the two
// sidebar-supported preferences. Anything else — including the raw
// group-level route_strategy vocabulary like "round_robin" — is
// rejected so the /admin/settings/routing surface stays a small,
// closed enum.
func IsValidRoutingPreference(s string) bool {
	switch RoutingPreference(s) {
	case RoutingPreferenceLoadBalance, RoutingPreferencePriority:
		return true
	default:
		return false
	}
}

// PreferenceToRouteStrategy maps the front-end preference vocabulary
// to a concrete domain.RouteStrategy used when creating new groups.
// Callers should already have validated the preference via
// IsValidRoutingPreference; an unknown value falls back to
// domain.RouteRoundRobin so a corrupted setting cannot land a group
// with an empty strategy.
func PreferenceToRouteStrategy(p RoutingPreference) domain.RouteStrategy {
	switch p {
	case RoutingPreferencePriority:
		return domain.RoutePriority
	case RoutingPreferenceLoadBalance:
		return domain.RouteRoundRobin
	default:
		return domain.RouteRoundRobin
	}
}

// ResolveDefaultRouteStrategy reads the operator-configured routing
// preference from the SettingsStore and maps it to a concrete
// domain.RouteStrategy. When the store is nil or the row is missing
// the fallback is domain.RouteRoundRobin — matching the sidebar's
// default radio state so a fresh deploy behaves the same before and
// after the setting is first written.
//
// The returned value is safe to persist directly on a Group.
func ResolveDefaultRouteStrategy(r *http.Request, store ports.SettingsStore) domain.RouteStrategy {
	if store == nil {
		return domain.RouteRoundRobin
	}
	v, err := store.Get(r.Context(), SettingKeyRoutingStrategyDefault)
	if err != nil {
		// ErrSettingNotFound is the only expected error here; anything
		// else (e.g. a DB read failure) also falls back to the safe
		// default so a transient adapter blip cannot 500 the create-
		// group path.
		return domain.RouteRoundRobin
	}
	if !IsValidRoutingPreference(v) {
		return domain.RouteRoundRobin
	}
	return PreferenceToRouteStrategy(RoutingPreference(v))
}

// RoutingSettingsHandler serves /admin/settings/routing endpoints. It
// owns a ports.SettingsStore reference — the same store used by the
// admin bearer flow — and reads/writes the routing_strategy_default
// key on top of the shared system_settings table.
type RoutingSettingsHandler struct {
	Settings ports.SettingsStore
}

// NewRoutingSettingsHandler wires a RoutingSettingsHandler over the
// given store. A nil store is rejected at construction time because
// every route here needs persistence — the caller (server.go) already
// gates the mount on Settings != nil.
func NewRoutingSettingsHandler(store ports.SettingsStore) *RoutingSettingsHandler {
	return &RoutingSettingsHandler{Settings: store}
}

// Register mounts the routing routes on the given /admin router.
func (h *RoutingSettingsHandler) Register(r chi.Router) {
	r.Get("/settings/routing", h.Get)
	r.Put("/settings/routing", h.Put)
}

// routingResponse is the JSON envelope for GET/PUT responses. The
// "source" field is "db" when the value was read from system_settings
// and "default" when the row was absent (or invalid) so the WebUI can
// render a "Using default" hint next to the radio.
type routingResponse struct {
	Strategy string `json:"strategy"`
	Source   string `json:"source"`
}

// routingRequest is the body shape for PUT /admin/settings/routing.
type routingRequest struct {
	Strategy string `json:"strategy"`
}

// Get returns the currently-configured routing preference. When no
// row exists the DefaultRoutingPreference is returned with
// source="default" — never 404 — so the WebUI does not have to
// disambiguate "never set" from "endpoint missing".
func (h *RoutingSettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	v, err := h.Settings.Get(r.Context(), SettingKeyRoutingStrategyDefault)
	if err != nil {
		if errors.Is(err, domain.ErrSettingNotFound) {
			writeJSON(w, http.StatusOK, routingResponse{
				Strategy: string(DefaultRoutingPreference),
				Source:   "default",
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !IsValidRoutingPreference(v) {
		// A row exists but its value is not one of the accepted
		// preferences — treat as if the row were missing so a bad
		// hand-edit of system_settings cannot brick the sidebar.
		writeJSON(w, http.StatusOK, routingResponse{
			Strategy: string(DefaultRoutingPreference),
			Source:   "default",
		})
		return
	}
	writeJSON(w, http.StatusOK, routingResponse{
		Strategy: v,
		Source:   "db",
	})
}

// Put replaces the routing preference. Body must be a JSON object
// with a "strategy" field set to one of the two accepted preferences;
// anything else 400s with error.type=invalid_strategy so the WebUI
// can render a targeted message.
func (h *RoutingSettingsHandler) Put(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req routingRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if !IsValidRoutingPreference(req.Strategy) {
		writeErr(w, http.StatusBadRequest, "invalid_strategy",
			`strategy must be "load_balance" or "priority"`)
		return
	}
	if err := h.Settings.Set(r.Context(), SettingKeyRoutingStrategyDefault, req.Strategy); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, routingResponse{
		Strategy: req.Strategy,
		Source:   "db",
	})
}
