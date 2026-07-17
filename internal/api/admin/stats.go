package admin

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// StatsHandler serves /admin/stats endpoints. It aggregates in-process so we
// don't have to add new store methods just for the dashboard.
type StatsHandler struct {
	Accounts ports.AccountStore
	Jobs     ports.JobStore // optional; may be nil
}

// NewStatsHandler wires a StatsHandler. Jobs is allowed to be nil.
func NewStatsHandler(a ports.AccountStore, j ports.JobStore) *StatsHandler {
	return &StatsHandler{Accounts: a, Jobs: j}
}

// Register mounts the routes under /admin/stats.
func (h *StatsHandler) Register(r chi.Router) {
	r.Get("/stats/pool", h.Pool)
	r.Get("/stats/health", h.Health)
}

// Pool returns pool-level aggregate stats: counts by plan and status, total
// subscription balance, and unlim-flag totals. Pulls the whole account list
// and aggregates in Go — fine for the pool sizes we care about (< a few
// thousand rows), and keeps AccountStore small.
func (h *StatsHandler) Pool(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Accounts.List(r.Context(), ports.AccountFilter{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	byPlan := map[string]int{}
	byStatus := map[string]int{}
	var totalSubBalance int64
	var withUnlim, withFlexUnlim int

	for i := range rows {
		a := &rows[i]
		byPlan[string(a.PlanType)]++
		byStatus[string(a.Status)]++
		totalSubBalance += a.SubscriptionBalance
		if a.HasUnlim {
			withUnlim++
		}
		if a.HasFlexUnlim {
			withFlexUnlim++
		}
	}

	// Convert credits*100 unit to whole credits for display.
	writeJSON(w, http.StatusOK, map[string]any{
		"total":                      len(rows),
		"by_plan":                    byPlan,
		"by_status":                  byStatus,
		"total_subscription_balance": float64(totalSubBalance) / 100.0,
		"with_unlim":                 withUnlim,
		"with_flex_unlim":            withFlexUnlim,
	})
}

// Health is a cheap liveness probe. It counts active accounts so operators
// can spot an empty pool without having to fetch /stats/pool.
func (h *StatsHandler) Health(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Accounts.List(r.Context(), ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"accounts_active": len(rows),
		"time":            time.Now().UTC().Format(time.RFC3339),
	})
}
