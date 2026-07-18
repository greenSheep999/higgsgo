package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	register "github.com/greensheep999/higgsgo/plugins/register"
)

type Handler struct {
	store  register.RegistrationStore
	worker *register.Worker
}

func NewHandler(store register.RegistrationStore, worker *register.Worker) *Handler {
	return &Handler{store: store, worker: worker}
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password,omitempty"`
		ProxyURL string `json:"proxy_url,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required")
		return
	}

	id, err := h.store.Enqueue(r.Context(), register.EnqueueRequest{
		Email:    req.Email,
		Password: req.Password,
		ProxyURL: req.ProxyURL,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Trigger the worker to pick it up immediately
	h.worker.Trigger(r.Context())

	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	filter := register.ListFilter{
		Limit:  50,
		Offset: 0,
	}

	if s := r.URL.Query().Get("status"); s != "" {
		status := register.RegistrationStatus(s)
		filter.Status = &status
	}

	regs, err := h.store.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, regs)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	reg, err := h.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if reg == nil {
		writeError(w, http.StatusNotFound, "registration not found")
		return
	}

	writeJSON(w, http.StatusOK, reg)
}

func (h *Handler) Retry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.store.Retry(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h.worker.Trigger(r.Context())

	writeJSON(w, http.StatusOK, map[string]string{"status": "queued"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
