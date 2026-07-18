package api

import "github.com/go-chi/chi/v5"

// Register mounts all registration routes on the given router.
func Register(r chi.Router, h *Handler) {
	r.Post("/registrations", h.Create)
	r.Get("/registrations", h.List)
	r.Get("/registrations/{id}", h.Get)
	r.Post("/registrations/{id}/retry", h.Retry)
}
