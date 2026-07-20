package admin

// LoadBalanceSettingsHandler serves /admin/settings/load_balance — six
// operator-editable knobs that steer the load_balance route strategy's
// internal ordering (tier-aware CASE, unlim / free_quota preference,
// richer-first tail, balance headroom multiplier, RANDOM() jitter).
//
// Persistence + defaults + parsing live in
// internal/core/loadbalance so both this handler (writer) and the
// proxy service (reader) share one source of truth without pulling
// core → api.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/loadbalance"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Re-exported for callers that need to reference the keys directly
// (typically tests that seed the store). New code should prefer
// loadbalance.Key* constants.
const (
	SettingKeyLoadBalanceTierAware          = loadbalance.KeyTierAware
	SettingKeyLoadBalancePreferUnlim        = loadbalance.KeyPreferUnlim
	SettingKeyLoadBalancePreferFreeQuota    = loadbalance.KeyPreferFreeQuota
	SettingKeyLoadBalancePreferRicher       = loadbalance.KeyPreferRicher
	SettingKeyLoadBalanceBalanceHeadroomPct = loadbalance.KeyBalanceHeadroomPct
	SettingKeyLoadBalanceJitter             = loadbalance.KeyJitter
)

// LoadBalanceSettingsHandler serves /admin/settings/load_balance. Owns
// a SettingsStore reference; nil is rejected at construction so the
// caller (server.go) must gate the mount on Settings != nil.
type LoadBalanceSettingsHandler struct {
	Settings ports.SettingsStore
}

// NewLoadBalanceSettingsHandler wires a handler over the given store.
func NewLoadBalanceSettingsHandler(store ports.SettingsStore) *LoadBalanceSettingsHandler {
	return &LoadBalanceSettingsHandler{Settings: store}
}

// Register mounts the load_balance routes on the given /admin router.
func (h *LoadBalanceSettingsHandler) Register(r chi.Router) {
	r.Get("/settings/load_balance", h.Get)
	r.Put("/settings/load_balance", h.Put)
}

// loadBalanceResponse is the GET / PUT response envelope. "source" is
// "db" when at least one key was present in system_settings and
// "default" when every key fell back — the WebUI can render a "using
// default" hint until the first save.
type loadBalanceResponse struct {
	loadbalance.Settings
	Source string `json:"source"`
}

// Get returns the currently-configured settings. Missing keys land on
// their defaults; a mixed history (some keys written, some not) still
// reports source="db" so the WebUI's "using default" hint disappears
// after the first save.
func (h *LoadBalanceSettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := loadbalance.Defaults()
	sawAny := false

	// Bool keys share a helper so the six-way expansion stays compact.
	boolReads := []struct {
		key string
		dst *bool
	}{
		{loadbalance.KeyTierAware, &out.TierAware},
		{loadbalance.KeyPreferUnlim, &out.PreferUnlim},
		{loadbalance.KeyPreferFreeQuota, &out.PreferFreeQuota},
		{loadbalance.KeyPreferRicher, &out.PreferRicher},
		{loadbalance.KeyJitter, &out.Jitter},
	}
	for _, br := range boolReads {
		v, err := h.Settings.Get(ctx, br.key)
		if err != nil {
			if errors.Is(err, domain.ErrSettingNotFound) {
				continue
			}
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if b, ok := loadbalance.ParseBool(v); ok {
			*br.dst = b
			sawAny = true
		}
	}

	if v, err := h.Settings.Get(ctx, loadbalance.KeyBalanceHeadroomPct); err == nil {
		if n, perr := strconv.Atoi(v); perr == nil && n >= loadbalance.HeadroomMin && n <= loadbalance.HeadroomMax {
			out.BalanceHeadroomPct = n
			sawAny = true
		}
	} else if !errors.Is(err, domain.ErrSettingNotFound) {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	source := "default"
	if sawAny {
		source = "db"
	}
	writeJSON(w, http.StatusOK, loadBalanceResponse{Settings: out, Source: source})
}

// Put replaces every load_balance.* key. The store contract is
// per-key, so a mid-write failure can leave a partial update — the GET
// path is defensive about this and falls back on any key it cannot
// parse.
//
// Validation: headroom must be in [HeadroomMin, HeadroomMax]. Anything
// else 400s with error.type=invalid_headroom so the WebUI can highlight
// the field. Bool fields are permissive — JSON parses "true"/"false"
// natively.
func (h *LoadBalanceSettingsHandler) Put(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_body", "body is required")
		return
	}
	var req loadbalance.Settings
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if req.BalanceHeadroomPct < loadbalance.HeadroomMin || req.BalanceHeadroomPct > loadbalance.HeadroomMax {
		writeErr(w, http.StatusBadRequest, "invalid_headroom",
			`balance_headroom_pct must be between 100 and 500`)
		return
	}

	ctx := r.Context()
	writes := []struct {
		key string
		val string
	}{
		{loadbalance.KeyTierAware, loadbalance.FormatBool(req.TierAware)},
		{loadbalance.KeyPreferUnlim, loadbalance.FormatBool(req.PreferUnlim)},
		{loadbalance.KeyPreferFreeQuota, loadbalance.FormatBool(req.PreferFreeQuota)},
		{loadbalance.KeyPreferRicher, loadbalance.FormatBool(req.PreferRicher)},
		{loadbalance.KeyBalanceHeadroomPct, strconv.Itoa(req.BalanceHeadroomPct)},
		{loadbalance.KeyJitter, loadbalance.FormatBool(req.Jitter)},
	}
	for _, kv := range writes {
		if err := h.Settings.Set(ctx, kv.key, kv.val); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, loadBalanceResponse{Settings: req, Source: "db"})
}
