package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/core/failover"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// FailoverHandler serves /admin/failover/* and the per-account
// /admin/accounts/{id}/failover surface.
//
// Cfg is treated as authoritative live state: PUT /admin/failover/config
// mutates the fields under a mutex so the failover Controller (which
// reads them without locking) always sees a coherent snapshot. Changes
// live in-memory only — a restart falls back to the TOML config, which
// is intentional MVP behaviour (persistent overrides live in the per-
// account overrides table).
type FailoverHandler struct {
	Accounts  ports.AccountStore
	Events    ports.FailoverEventStore
	Overrides ports.FailoverOverridesStore
	Cfg       *config.FailoverConfig // shared with the Controller; guarded by mu
	Logger    *slog.Logger

	mu sync.Mutex
}

// NewFailoverHandler wires the handler. Any of the store dependencies
// may be nil in slimmer deployments — the affected endpoints return
// 503 unavailable so operators get a clear signal.
func NewFailoverHandler(accounts ports.AccountStore, events ports.FailoverEventStore, overrides ports.FailoverOverridesStore, cfg *config.FailoverConfig, logger *slog.Logger) *FailoverHandler {
	return &FailoverHandler{
		Accounts:  accounts,
		Events:    events,
		Overrides: overrides,
		Cfg:       cfg,
		Logger:    logger,
	}
}

// Register mounts the failover routes. The /admin/accounts/{id}/failover
// endpoints are mounted here (rather than in accounts.go) so all failover
// wiring stays in one place and the AccountsHandler doesn't need to grow
// dependencies on the failover stores.
func (h *FailoverHandler) Register(r chi.Router) {
	r.Get("/failover/config", h.GetConfig)
	r.Put("/failover/config", h.PutConfig)
	r.Get("/failover/isolated", h.ListIsolated)
	r.Get("/accounts/{id}/failover", h.GetAccountFailover)
	r.Put("/accounts/{id}/failover", h.PutAccountFailover)
	r.Post("/accounts/{id}/recover", h.RecoverAccount)
}

// GetConfig returns the current in-memory FailoverConfig snapshot.
// Safe against concurrent mutations by acquiring the same mutex the
// PUT path uses.
func (h *FailoverHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	if h.Cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "failover subsystem not configured")
		return
	}
	h.mu.Lock()
	snapshot := *h.Cfg
	// Deep-copy the risk marker slice so an out-of-band mutation
	// through the caller's response body cannot alias the live config.
	if len(snapshot.Throttle.RiskMarkers) > 0 {
		copied := make([]string, len(snapshot.Throttle.RiskMarkers))
		copy(copied, snapshot.Throttle.RiskMarkers)
		snapshot.Throttle.RiskMarkers = copied
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, failoverConfigView(&snapshot))
}

// PutConfig accepts a partial JSON body and mutates the live Cfg. Any
// omitted field is left unchanged so the caller can flip a single flag
// without echoing the whole config back. Returns the resulting snapshot.
func (h *FailoverHandler) PutConfig(w http.ResponseWriter, r *http.Request) {
	if h.Cfg == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "failover subsystem not configured")
		return
	}
	var body failoverConfigPatch
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	h.mu.Lock()
	if body.Enabled != nil {
		h.Cfg.Enabled = *body.Enabled
	}
	if body.Consecutive != nil {
		if body.Consecutive.Enabled != nil {
			h.Cfg.Consecutive.Enabled = *body.Consecutive.Enabled
		}
		if body.Consecutive.FailLimit != nil {
			h.Cfg.Consecutive.FailLimit = *body.Consecutive.FailLimit
		}
	}
	if body.Throttle != nil {
		if body.Throttle.Enabled != nil {
			h.Cfg.Throttle.Enabled = *body.Throttle.Enabled
		}
		if body.Throttle.JudgeWindowSec != nil {
			h.Cfg.Throttle.JudgeWindowSec = *body.Throttle.JudgeWindowSec
		}
		if body.Throttle.JudgeCount != nil {
			h.Cfg.Throttle.JudgeCount = *body.Throttle.JudgeCount
		}
		if body.Throttle.CooldownSec != nil {
			h.Cfg.Throttle.CooldownSec = *body.Throttle.CooldownSec
		}
		if body.Throttle.EvictWindowSec != nil {
			h.Cfg.Throttle.EvictWindowSec = *body.Throttle.EvictWindowSec
		}
		if body.Throttle.EvictCount != nil {
			h.Cfg.Throttle.EvictCount = *body.Throttle.EvictCount
		}
		if body.Throttle.RiskMarkers != nil {
			copied := make([]string, len(*body.Throttle.RiskMarkers))
			copy(copied, *body.Throttle.RiskMarkers)
			h.Cfg.Throttle.RiskMarkers = copied
		}
	}
	if body.OutageGuard != nil {
		if body.OutageGuard.WindowSec != nil {
			h.Cfg.OutageGuard.WindowSec = *body.OutageGuard.WindowSec
		}
		if body.OutageGuard.DisableCountLimit != nil {
			h.Cfg.OutageGuard.DisableCountLimit = *body.OutageGuard.DisableCountLimit
		}
	}
	snapshot := *h.Cfg
	if len(snapshot.Throttle.RiskMarkers) > 0 {
		copied := make([]string, len(snapshot.Throttle.RiskMarkers))
		copy(copied, snapshot.Throttle.RiskMarkers)
		snapshot.Throttle.RiskMarkers = copied
	}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, failoverConfigView(&snapshot))
}

// ListIsolated returns every account whose status is throttled or
// disabled, plus enough context (status_reason, throttled_until, recent
// event count) that the admin surface can render a quick incident list
// without a per-row round trip. Read-only.
func (h *FailoverHandler) ListIsolated(w http.ResponseWriter, r *http.Request) {
	if h.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "accounts store not configured")
		return
	}
	ctx := r.Context()
	throttled, err := h.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusThrottled})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	disabled, err := h.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusDisabled})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	rows := make([]map[string]any, 0, len(throttled)+len(disabled))
	for _, a := range throttled {
		rows = append(rows, isolatedRowView(&a, h.eventCount(ctx, a.ID)))
	}
	for _, a := range disabled {
		rows = append(rows, isolatedRowView(&a, h.eventCount(ctx, a.ID)))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(rows),
		"data":  rows,
	})
}

// GetAccountFailover returns per-account failover context: windowed
// event counts, the current effective override, and the last N events.
func (h *FailoverHandler) GetAccountFailover(w http.ResponseWriter, r *http.Request) {
	if h.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "accounts store not configured")
		return
	}
	id := chi.URLParam(r, "id")
	acc, err := h.Accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	body := map[string]any{
		"account": map[string]any{
			"id":              acc.ID,
			"email":           acc.Email,
			"status":          string(acc.Status),
			"status_reason":   acc.StatusReason,
			"fail_streak":     acc.FailStreak,
			"throttled_until": timeOrNil(acc.ThrottledUntil),
		},
	}

	if h.Overrides != nil {
		o, err := h.Overrides.Get(r.Context(), id)
		if err == nil && o != nil {
			body["override"] = failoverOverrideView(o)
		}
	}
	if h.Events != nil && h.Cfg != nil {
		body["counts"] = map[string]int{
			"failure_last_hour":  safeCount(r.Context(), h.Events, id, ports.FailoverEventFailure, 3600),
			"throttle_in_judge":  safeCount(r.Context(), h.Events, id, ports.FailoverEventThrottle, h.Cfg.Throttle.JudgeWindowSec),
			"blacklist_in_evict": safeCount(r.Context(), h.Events, id, ports.FailoverEventBlacklist, h.Cfg.Throttle.EvictWindowSec),
		}
		events, err := h.Events.List(r.Context(), id, 50)
		if err == nil {
			eventViews := make([]map[string]any, 0, len(events))
			for _, e := range events {
				eventViews = append(eventViews, map[string]any{
					"id":          e.ID,
					"kind":        string(e.Kind),
					"reason":      e.Reason,
					"http_status": e.HTTPStatus,
					"created_at":  e.CreatedAt.Format(time.RFC3339),
				})
			}
			body["events"] = eventViews
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// PutAccountFailover replaces the per-account override. Every field is
// a pointer so a partial write can NULL an override back to "inherit
// global" by explicitly omitting the field (or setting it to null).
func (h *FailoverHandler) PutAccountFailover(w http.ResponseWriter, r *http.Request) {
	if h.Overrides == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "failover overrides store not configured")
		return
	}
	id := chi.URLParam(r, "id")
	// Confirm the account exists so we don't upsert an override for a
	// bogus id.
	if h.Accounts != nil {
		if _, err := h.Accounts.Get(r.Context(), id); err != nil {
			if errors.Is(err, domain.ErrAccountNotFound) {
				writeErr(w, http.StatusNotFound, "not_found", "account not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	var body failoverOverridePatch
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	o := &ports.FailoverOverride{
		AccountID:      id,
		Enabled:        body.Enabled,
		FailLimit:      body.FailLimit,
		JudgeWindowSec: body.JudgeWindowSec,
		JudgeCount:     body.JudgeCount,
		CooldownSec:    body.CooldownSec,
		EvictWindowSec: body.EvictWindowSec,
		EvictCount:     body.EvictCount,
	}
	if err := h.Overrides.Upsert(r.Context(), o); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, failoverOverrideView(o))
}

// RecoverAccount brings a disabled account back to active. It also
// clears the accumulated events so the account starts the next window
// with a clean slate (otherwise the very next failure would trip the
// evict edge immediately). Only accepts disabled → active transitions;
// throttled accounts recover automatically via the Recoverer goroutine.
func (h *FailoverHandler) RecoverAccount(w http.ResponseWriter, r *http.Request) {
	if h.Accounts == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "accounts store not configured")
		return
	}
	id := chi.URLParam(r, "id")
	acc, err := h.Accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if acc.Status != domain.StatusDisabled && acc.Status != domain.StatusThrottled {
		writeErr(w, http.StatusConflict, "not_disabled",
			"account is not disabled/throttled — nothing to recover")
		return
	}
	if err := h.Accounts.MarkStatus(r.Context(), id, domain.StatusActive, failover.ReasonManualRecover); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Clear fail streak explicitly — MarkStatus preserves it on active
	// transitions so the failover controller doesn't fight the caller.
	if err := h.Accounts.ResetFailStreak(r.Context(), id); err != nil && h.Logger != nil {
		h.Logger.Warn("failover recover: reset fail_streak failed",
			slog.String("account_id", id),
			slog.String("err", err.Error()))
	}
	if h.Events != nil {
		if err := h.Events.DeleteForAccount(r.Context(), id); err != nil && h.Logger != nil {
			h.Logger.Warn("failover recover: delete events failed",
				slog.String("account_id", id),
				slog.String("err", err.Error()))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": id,
		"status":     string(domain.StatusActive),
	})
}

// eventCount returns the number of failure events for the given
// account inside the operator-configured evict window. Used only for
// the isolated-list summary; a store error surfaces as 0 so the row
// still renders.
func (h *FailoverHandler) eventCount(ctx context.Context, accountID string) int {
	if h.Events == nil || h.Cfg == nil {
		return 0
	}
	window := h.Cfg.Throttle.EvictWindowSec
	if window <= 0 {
		window = 3600
	}
	n, err := h.Events.Count(ctx, accountID, ports.FailoverEventFailure, window)
	if err != nil {
		return 0
	}
	return n
}

// --- shapes ------------------------------------------------------------

type failoverConfigPatch struct {
	Enabled     *bool                     `json:"enabled,omitempty"`
	Consecutive *consecutiveFailoverPatch `json:"consecutive,omitempty"`
	Throttle    *throttleFailoverPatch    `json:"throttle,omitempty"`
	OutageGuard *outageGuardPatch         `json:"outage_guard,omitempty"`
}

type consecutiveFailoverPatch struct {
	Enabled   *bool `json:"enabled,omitempty"`
	FailLimit *int  `json:"fail_limit,omitempty"`
}

type throttleFailoverPatch struct {
	Enabled        *bool     `json:"enabled,omitempty"`
	JudgeWindowSec *int      `json:"judge_window_sec,omitempty"`
	JudgeCount     *int      `json:"judge_count,omitempty"`
	CooldownSec    *int      `json:"cooldown_sec,omitempty"`
	EvictWindowSec *int      `json:"evict_window_sec,omitempty"`
	EvictCount     *int      `json:"evict_count,omitempty"`
	RiskMarkers    *[]string `json:"risk_markers,omitempty"`
}

type outageGuardPatch struct {
	WindowSec         *int `json:"window_sec,omitempty"`
	DisableCountLimit *int `json:"disable_count_limit,omitempty"`
}

type failoverOverridePatch struct {
	Enabled        *bool `json:"enabled,omitempty"`
	FailLimit      *int  `json:"fail_limit,omitempty"`
	JudgeWindowSec *int  `json:"judge_window_sec,omitempty"`
	JudgeCount     *int  `json:"judge_count,omitempty"`
	CooldownSec    *int  `json:"cooldown_sec,omitempty"`
	EvictWindowSec *int  `json:"evict_window_sec,omitempty"`
	EvictCount     *int  `json:"evict_count,omitempty"`
}

func failoverConfigView(cfg *config.FailoverConfig) map[string]any {
	return map[string]any{
		"enabled": cfg.Enabled,
		"consecutive": map[string]any{
			"enabled":    cfg.Consecutive.Enabled,
			"fail_limit": cfg.Consecutive.FailLimit,
		},
		"throttle": map[string]any{
			"enabled":          cfg.Throttle.Enabled,
			"judge_window_sec": cfg.Throttle.JudgeWindowSec,
			"judge_count":      cfg.Throttle.JudgeCount,
			"cooldown_sec":     cfg.Throttle.CooldownSec,
			"evict_window_sec": cfg.Throttle.EvictWindowSec,
			"evict_count":      cfg.Throttle.EvictCount,
			"risk_markers":     cfg.Throttle.RiskMarkers,
		},
		"outage_guard": map[string]any{
			"window_sec":          cfg.OutageGuard.WindowSec,
			"disable_count_limit": cfg.OutageGuard.DisableCountLimit,
		},
	}
}

func failoverOverrideView(o *ports.FailoverOverride) map[string]any {
	m := map[string]any{
		"account_id": o.AccountID,
	}
	if o.Enabled != nil {
		m["enabled"] = *o.Enabled
	}
	if o.FailLimit != nil {
		m["fail_limit"] = *o.FailLimit
	}
	if o.JudgeWindowSec != nil {
		m["judge_window_sec"] = *o.JudgeWindowSec
	}
	if o.JudgeCount != nil {
		m["judge_count"] = *o.JudgeCount
	}
	if o.CooldownSec != nil {
		m["cooldown_sec"] = *o.CooldownSec
	}
	if o.EvictWindowSec != nil {
		m["evict_window_sec"] = *o.EvictWindowSec
	}
	if o.EvictCount != nil {
		m["evict_count"] = *o.EvictCount
	}
	if !o.UpdatedAt.IsZero() {
		m["updated_at"] = o.UpdatedAt.Format(time.RFC3339)
	}
	return m
}

func isolatedRowView(a *domain.Account, events int) map[string]any {
	m := map[string]any{
		"id":               a.ID,
		"email":            a.Email,
		"plan_type":        string(a.PlanType),
		"status":           string(a.Status),
		"status_reason":    a.StatusReason,
		"fail_streak":      a.FailStreak,
		"events_in_window": events,
	}
	if !a.ThrottledUntil.IsZero() {
		m["throttled_until"] = a.ThrottledUntil.Format(time.RFC3339)
	}
	if !a.LastFailedAt.IsZero() {
		m["last_failed_at"] = a.LastFailedAt.Format(time.RFC3339)
	}
	return m
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

func safeCount(ctx context.Context, store ports.FailoverEventStore, id string, kind ports.FailoverEventKind, windowSec int) int {
	if store == nil || windowSec <= 0 {
		return 0
	}
	n, err := store.Count(ctx, id, kind, windowSec)
	if err != nil {
		return 0
	}
	return n
}
