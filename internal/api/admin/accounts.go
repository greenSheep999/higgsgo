package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// AccountsHandler serves /admin/accounts endpoints. It never leaks
// password_enc / cookies_json values back to clients — those columns are
// sensitive at rest and only the pool internals should ever read them.
type AccountsHandler struct {
	Accounts ports.AccountStore
}

// NewAccountsHandler wires an AccountsHandler over the given store.
func NewAccountsHandler(a ports.AccountStore) *AccountsHandler {
	return &AccountsHandler{Accounts: a}
}

// Register mounts the routes under /admin/accounts.
func (h *AccountsHandler) Register(r chi.Router) {
	r.Get("/accounts", h.List)
	r.Get("/accounts/{id}", h.Get)
	r.Post("/accounts/{id}/pause", h.Pause)
	r.Post("/accounts/{id}/resume", h.Resume)
	r.Delete("/accounts/{id}", h.SoftDelete)
}

// List returns all accounts, optionally filtered by ?plan_type=, ?status=,
// ?min_balance= query parameters. Sensitive fields are stripped.
func (h *AccountsHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := ports.AccountFilter{
		PlanType: domain.PlanType(q.Get("plan_type")),
		Status:   domain.AccountStatus(q.Get("status")),
	}
	if raw := q.Get("min_balance"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_query", "min_balance must be an integer")
			return
		}
		filter.MinBalance = v
	}
	rows, err := h.Accounts.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, accountView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// Get returns one account by id (sensitive fields stripped).
func (h *AccountsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := h.Accounts.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, accountView(a))
}

// Pause flips the account status to "suspended". The pool skips suspended
// accounts on PickAndLock, so this is the safe way to take an account out of
// rotation without losing its credentials.
func (h *AccountsHandler) Pause(w http.ResponseWriter, r *http.Request) {
	h.markStatus(w, r, domain.StatusSuspended, "manual pause")
}

// Resume flips the account status back to "active".
func (h *AccountsHandler) Resume(w http.ResponseWriter, r *http.Request) {
	h.markStatus(w, r, domain.StatusActive, "manual resume")
}

// SoftDelete flips the account status to "banned". Rows are never physically
// removed — this keeps the audit trail (jobs, usage_events) intact.
func (h *AccountsHandler) SoftDelete(w http.ResponseWriter, r *http.Request) {
	h.markStatus(w, r, domain.StatusBanned, "manual delete")
}

func (h *AccountsHandler) markStatus(w http.ResponseWriter, r *http.Request, status domain.AccountStatus, reason string) {
	id := chi.URLParam(r, "id")
	if err := h.Accounts.MarkStatus(r.Context(), id, status, reason); err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "account not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     id,
		"status": string(status),
		"reason": reason,
	})
}

// accountView is the public-safe representation of an Account. Password and
// cookies are never included; UA / DataDome / session id are also treated as
// secrets since together they let an attacker impersonate the account.
func accountView(a *domain.Account) map[string]any {
	v := map[string]any{
		"id":                   a.ID,
		"email":                a.Email,
		"workspace_id":         a.WorkspaceID,
		"plan_type":            string(a.PlanType),
		"has_unlim":            a.HasUnlim,
		"has_flex_unlim":       a.HasFlexUnlim,
		"is_pro_veo3":          a.IsProVeo3Available,
		"cohort":               a.Cohort,
		"subscription_balance": a.SubscriptionBalance,
		"credits_balance":      a.CreditsBalance,
		"total_plan_credits":   a.TotalPlanCredits,
		"status":               string(a.Status),
		"in_flight_jobs":       a.InFlightJobs,
		"fail_streak":          a.FailStreak,
		"bound_proxy_url":      a.BoundProxyURL,
	}
	if !a.PlanEndsAt.IsZero() {
		v["plan_ends_at"] = a.PlanEndsAt.UTC().Format(time.RFC3339)
	}
	if !a.LastBalanceAt.IsZero() {
		v["last_balance_at"] = a.LastBalanceAt.UTC().Format(time.RFC3339)
	}
	if !a.LastUsedAt.IsZero() {
		v["last_used_at"] = a.LastUsedAt.UTC().Format(time.RFC3339)
	}
	if !a.LastFailedAt.IsZero() {
		v["last_failed_at"] = a.LastFailedAt.UTC().Format(time.RFC3339)
	}
	if !a.RegisteredAt.IsZero() {
		v["registered_at"] = a.RegisteredAt.UTC().Format(time.RFC3339)
	}
	if !a.ImportedAt.IsZero() {
		v["imported_at"] = a.ImportedAt.UTC().Format(time.RFC3339)
	}
	return v
}
