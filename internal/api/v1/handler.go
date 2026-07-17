// Package v1 hosts the OpenAI-compatible public API surface.
//
// Endpoints:
//
//	GET  /v1/models                     list of usable model aliases
//	GET  /v1/models/{alias}             single-model detail
//	POST /v1/videos/generations         create a video job
//	POST /v1/images/generations         create an image job
//	POST /v1/audio/generations          create an audio (TTS) job
//	GET  /v1/jobs/{id}                  poll an async job
package v1

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Handler wires v1 endpoints to the reverse-proxy service.
type Handler struct {
	Service  *proxy.Service
	Registry ports.ModelRegistry
	Jobs     ports.JobStore // optional; when nil, /v1/jobs/{id} returns 503
}

// New builds a Handler.
func New(svc *proxy.Service, reg ports.ModelRegistry, jobs ports.JobStore) *Handler {
	return &Handler{Service: svc, Registry: reg, Jobs: jobs}
}

// HandleModelsList serves GET /v1/models.
func (h *Handler) HandleModelsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := ports.ModelFilter{
		Output:            q.Get("output"),
		IncludeUnstable:   q.Get("include_unstable") == "1",
		IncludeDeprecated: q.Get("include_deprecated") == "1",
	}
	models := h.Registry.List(filter)

	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id":              m.Alias,
			"object":          "model",
			"output":          m.Output,
			"jst":             m.JST,
			"est_cost":        float64(m.EstCostHundredths) / 100.0,
			"required_params": m.RequiredParams,
			"unstable":        m.Unstable,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// HandleModelDetail serves GET /v1/models/{alias}.
func (h *Handler) HandleModelDetail(w http.ResponseWriter, r *http.Request) {
	alias := chi.URLParam(r, "alias")
	spec, err := h.Registry.Resolve(alias)
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

// --- helpers ------------------------------------------------------------

func readAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    kind,
			"message": message,
		},
	})
}
