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
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Handler wires v1 endpoints to the reverse-proxy service.
type Handler struct {
	Service  *proxy.Service
	Registry ports.ModelRegistry
	Jobs     ports.JobStore   // optional; when nil, /v1/jobs/{id} returns 503
	Groups   ports.GroupStore // optional; when non-nil, missing group_id is auto-resolved from the api key's bindings
	Logger   *slog.Logger     // optional; used for warnings during best-effort auto-resolution
}

// New builds a Handler. The groups argument is optional and enables the
// api-key → group auto-resolution behaviour for /v1 generation endpoints.
func New(svc *proxy.Service, reg ports.ModelRegistry, jobs ports.JobStore, groups ports.GroupStore) *Handler {
	return &Handler{Service: svc, Registry: reg, Jobs: jobs, Groups: groups}
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

// httpError describes a client-facing error produced by internal helpers so
// the caller (an HTTP handler) can render it via writeError. It is not an
// exported type because it is only used to hand results back within the
// package.
type httpError struct {
	Status  int
	Kind    string
	Message string
}

// resolveGroup returns the group ID that should scope a /v1 generation
// request. If the caller explicitly set a group in the request body, that
// value is returned verbatim. Otherwise, when the handler has a GroupStore,
// the api key's bindings are queried:
//
//   - zero bindings: returns "" and no error (caller uses the default group).
//   - one binding:   returns that group's ID.
//   - multiple:      returns an ambiguous_group httpError so the caller has
//     to disambiguate by setting group_id explicitly.
//
// Store errors are treated as best-effort failures: they are logged (when
// logger is non-nil) but not surfaced to the caller — the request continues
// with an empty group scope.
func resolveGroup(ctx context.Context, groups ports.GroupStore, logger *slog.Logger, apiKeyID, requested string) (string, *httpError) {
	if requested != "" {
		return requested, nil
	}
	if groups == nil || apiKeyID == "" {
		return "", nil
	}
	bound, err := groups.ListGroupsForAPIKey(ctx, apiKeyID)
	if err != nil {
		if logger != nil {
			logger.Warn("group resolve failed",
				slog.String("api_key_id", apiKeyID),
				slog.String("err", err.Error()),
			)
		}
		return "", nil
	}
	switch len(bound) {
	case 0:
		return "", nil
	case 1:
		return bound[0].ID, nil
	default:
		return "", &httpError{
			Status:  http.StatusBadRequest,
			Kind:    "ambiguous_group",
			Message: "api key is bound to multiple groups; please specify group_id in the request",
		}
	}
}
