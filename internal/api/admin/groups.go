package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// GroupsHandler serves /admin/groups endpoints. It manages pool
// subdivisions (account groups), the accounts that belong to them, and the
// api-key-to-group bindings that gate which callers may consume from which
// pool subset.
type GroupsHandler struct {
	Groups ports.GroupStore

	// Settings is an optional dependency used only by Create to look up
	// the operator-configured routing_strategy_default when the request
	// body does not carry an explicit route_strategy. Nil is safe — the
	// handler falls back to domain.RouteRoundRobin, matching the prior
	// hard-coded behaviour.
	Settings ports.SettingsStore
}

// NewGroupsHandler wires a GroupsHandler over the given store. The
// SettingsStore-dependent default routing behaviour is opt-in via the
// public Settings field; slimmer deployments that skip the settings
// surface still get sane defaults.
func NewGroupsHandler(g ports.GroupStore) *GroupsHandler {
	return &GroupsHandler{Groups: g}
}

// Register mounts the routes under /admin/groups.
func (h *GroupsHandler) Register(r chi.Router) {
	r.Get("/groups", h.List)
	r.Post("/groups", h.Create)
	r.Get("/groups/{id}", h.Get)
	r.Put("/groups/{id}", h.Update)
	r.Delete("/groups/{id}", h.Delete)

	r.Get("/groups/{id}/members", h.ListMembers)
	r.Post("/groups/{id}/members", h.AddMember)
	r.Delete("/groups/{id}/members/{accountId}", h.RemoveMember)

	r.Get("/groups/{id}/bindings", h.ListBindings)
	r.Post("/groups/{id}/bindings", h.BindAPIKey)
	r.Delete("/groups/{id}/bindings/{apiKeyId}", h.UnbindAPIKey)
}

// createGroupRequest is the body shape for POST /admin/groups.
type createGroupRequest struct {
	Name                    string `json:"name"`
	Description             string `json:"description,omitempty"`
	MaxConcurrentJobs       int    `json:"max_concurrent_jobs,omitempty"`
	MaxConcurrentPerAccount int    `json:"max_concurrent_per_account,omitempty"`
	MonthlyCreditBudget     int64  `json:"monthly_credit_budget,omitempty"` // credits × 100
	AllowedModelsRegex      string `json:"allowed_models_regex,omitempty"`
	BlockedModelsRegex      string `json:"blocked_models_regex,omitempty"`
	RouteStrategy           string `json:"route_strategy,omitempty"` // e.g., round_robin
	OwnerType               string `json:"owner_type,omitempty"`     // apikey | cpa_partner | internal
	OwnerID                 string `json:"owner_id,omitempty"`
}

// List returns every account_groups row, alphabetized by name.
func (h *GroupsHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Groups.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for i := range rows {
		data = append(data, groupView(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// Create inserts a new group. Name is required; everything else defaults.
func (h *GroupsHandler) Create(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req createGroupRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "name is required")
		return
	}
	ownerType := domain.OwnerType(req.OwnerType)
	if ownerType == "" {
		ownerType = domain.OwnerInternal
	}
	route := domain.RouteStrategy(req.RouteStrategy)
	if route == "" {
		// No explicit strategy on the request → consult the operator-
		// configured routing_strategy_default in system_settings. When
		// the store is nil, the row is missing, or the persisted value
		// is unrecognised, ResolveDefaultRouteStrategy falls back to
		// domain.RouteRoundRobin so this path degrades to the prior
		// hard-coded behaviour.
		route = ResolveDefaultRouteStrategy(r, h.Settings)
	}
	g := &domain.Group{
		ID:                      idgen.NewID("grp"),
		Name:                    req.Name,
		Description:             req.Description,
		MaxConcurrentJobs:       req.MaxConcurrentJobs,
		MaxConcurrentPerAccount: req.MaxConcurrentPerAccount,
		MonthlyCreditBudget:     req.MonthlyCreditBudget,
		AllowedModelsRegex:      req.AllowedModelsRegex,
		BlockedModelsRegex:      req.BlockedModelsRegex,
		RouteStrategy:           route,
		OwnerType:               ownerType,
		OwnerID:                 req.OwnerID,
		Status:                  "active",
		CreatedAt:               time.Now().UTC(),
	}
	if err := h.Groups.Create(r.Context(), g); err != nil {
		writeErr(w, http.StatusInternalServerError, "insert", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, groupView(g))
}

// updateGroupRequest is the body shape for PUT /admin/groups/{id}. All
// fields are optional pointers so callers can send a partial update; a
// nil pointer leaves the existing value untouched.
type updateGroupRequest struct {
	Name                    *string `json:"name,omitempty"`
	Description             *string `json:"description,omitempty"`
	MaxConcurrentJobs       *int    `json:"max_concurrent_jobs,omitempty"`
	MaxConcurrentPerAccount *int    `json:"max_concurrent_per_account,omitempty"`
	MonthlyCreditBudget     *int64  `json:"monthly_credit_budget,omitempty"`
	AllowedModelsRegex      *string `json:"allowed_models_regex,omitempty"`
	BlockedModelsRegex      *string `json:"blocked_models_regex,omitempty"`
	RouteStrategy           *string `json:"route_strategy,omitempty"`
	OwnerType               *string `json:"owner_type,omitempty"`
	OwnerID                 *string `json:"owner_id,omitempty"`
	Status                  *string `json:"status,omitempty"`
}

// Update overwrites the mutable fields of an existing group. Load-then-
// merge so callers can PATCH-style send a partial body without wiping
// unrelated fields.
func (h *GroupsHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	existing, err := h.Groups.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrGroupNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "group not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, 8192))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req updateGroupRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}

	if req.Name != nil {
		if *req.Name == "" {
			writeErr(w, http.StatusBadRequest, "invalid_body", "name must not be empty")
			return
		}
		existing.Name = *req.Name
	}
	if req.Description != nil {
		existing.Description = *req.Description
	}
	if req.MaxConcurrentJobs != nil {
		existing.MaxConcurrentJobs = *req.MaxConcurrentJobs
	}
	if req.MaxConcurrentPerAccount != nil {
		existing.MaxConcurrentPerAccount = *req.MaxConcurrentPerAccount
	}
	if req.MonthlyCreditBudget != nil {
		existing.MonthlyCreditBudget = *req.MonthlyCreditBudget
	}
	if req.AllowedModelsRegex != nil {
		existing.AllowedModelsRegex = *req.AllowedModelsRegex
	}
	if req.BlockedModelsRegex != nil {
		existing.BlockedModelsRegex = *req.BlockedModelsRegex
	}
	if req.RouteStrategy != nil {
		existing.RouteStrategy = domain.RouteStrategy(*req.RouteStrategy)
	}
	if req.OwnerType != nil {
		existing.OwnerType = domain.OwnerType(*req.OwnerType)
	}
	if req.OwnerID != nil {
		existing.OwnerID = *req.OwnerID
	}
	if req.Status != nil && *req.Status != "" {
		existing.Status = *req.Status
	}

	if err := h.Groups.Update(r.Context(), existing); err != nil {
		if errors.Is(err, domain.ErrGroupNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "group not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupView(existing))
}

// Get returns one group by id.
func (h *GroupsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	g, err := h.Groups.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrGroupNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "group not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupView(g))
}

// Delete removes a group. FK cascades on members / bindings clean up too.
func (h *GroupsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Groups.Delete(r.Context(), id); err != nil {
		if errors.Is(err, domain.ErrGroupNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "group not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "deleted"})
}

// ListMembers returns the account ids attached to a group.
func (h *GroupsHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ids, err := h.Groups.ListMembers(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"group_id": id, "members": ids})
}

// addMemberRequest is the body shape for POST /admin/groups/{id}/members.
type addMemberRequest struct {
	AccountID string `json:"account_id"`
	Priority  int    `json:"priority,omitempty"`
}

// AddMember attaches an account to a group. Idempotent.
func (h *GroupsHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req addMemberRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.AccountID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "account_id is required")
		return
	}
	if err := h.Groups.AddMember(r.Context(), id, req.AccountID, req.Priority); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group_id":   id,
		"account_id": req.AccountID,
		"priority":   req.Priority,
	})
}

// RemoveMember detaches an account from a group.
func (h *GroupsHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	accountID := chi.URLParam(r, "accountId")
	if err := h.Groups.RemoveMember(r.Context(), id, accountID); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group_id":   id,
		"account_id": accountID,
		"status":     "removed",
	})
}

// ListBindings returns the api key ids bound to a group.
func (h *GroupsHandler) ListBindings(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ids, err := h.Groups.ListAPIKeys(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": ids})
}

// bindKeyRequest is the body shape for POST /admin/groups/{id}/bindings.
type bindKeyRequest struct {
	APIKeyID string `json:"api_key_id"`
}

// BindAPIKey allows an api key to consume accounts from a group.
func (h *GroupsHandler) BindAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var req bindKeyRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	if req.APIKeyID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "api_key_id is required")
		return
	}
	if err := h.Groups.BindAPIKey(r.Context(), req.APIKeyID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group_id":   id,
		"api_key_id": req.APIKeyID,
		"status":     "bound",
	})
}

// UnbindAPIKey removes an api-key-to-group binding.
func (h *GroupsHandler) UnbindAPIKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	apiKeyID := chi.URLParam(r, "apiKeyId")
	if err := h.Groups.UnbindAPIKey(r.Context(), apiKeyID, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group_id":   id,
		"api_key_id": apiKeyID,
		"status":     "unbound",
	})
}

// groupView is the public-safe JSON representation of a Group.
func groupView(g *domain.Group) map[string]any {
	v := map[string]any{
		"id":                         g.ID,
		"name":                       g.Name,
		"description":                g.Description,
		"max_concurrent_jobs":        g.MaxConcurrentJobs,
		"max_concurrent_per_account": g.MaxConcurrentPerAccount,
		"monthly_credit_budget":      g.MonthlyCreditBudget,
		"monthly_credit_used":        g.MonthlyCreditUsed,
		"allowed_models_regex":       g.AllowedModelsRegex,
		"blocked_models_regex":       g.BlockedModelsRegex,
		"route_strategy":             string(g.RouteStrategy),
		"owner_type":                 string(g.OwnerType),
		"owner_id":                   g.OwnerID,
		"status":                     g.Status,
	}
	if !g.CreatedAt.IsZero() {
		v["created_at"] = g.CreatedAt.UTC().Format(time.RFC3339)
	}
	return v
}
