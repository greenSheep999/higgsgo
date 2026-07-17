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
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// defaultModelsListLimit / maxModelsListLimit mirror the pagination caps
// used by /v1/jobs so client-side paging behaviour is consistent across
// the surface.
const (
	defaultModelsListLimit = 100
	maxModelsListLimit     = 500
)

// Handler wires v1 endpoints to the reverse-proxy service.
type Handler struct {
	Service  *proxy.Service
	Registry ports.ModelRegistry
	Jobs     ports.JobStore    // optional; when nil, /v1/jobs/{id} returns 503
	Groups   ports.GroupStore  // optional; when non-nil, missing group_id is auto-resolved from the api key's bindings
	APIKeys  ports.APIKeyStore // optional; enables the api_keys.group_id direct 1:1 binding shortcut in resolveGroup
	Logger   *slog.Logger      // optional; used for warnings during best-effort auto-resolution
}

// New builds a Handler. The groups argument is optional and enables the
// api-key → group auto-resolution behaviour for /v1 generation endpoints.
// The apiKeys argument is also optional and enables the api_keys.group_id
// direct 1:1 binding shortcut: when the caller's API key row carries a
// non-empty GroupID, resolveGroup returns it without consulting Groups.
func New(svc *proxy.Service, reg ports.ModelRegistry, jobs ports.JobStore, groups ports.GroupStore, apiKeys ports.APIKeyStore) *Handler {
	return &Handler{Service: svc, Registry: reg, Jobs: jobs, Groups: groups, APIKeys: apiKeys}
}

// HandleModelsList serves GET /v1/models.
//
// Supported query parameters:
//
//	output=image|video|audio       filter by ModelSpec.Output
//	requires_paid=true|false       filter by RequiresPaid flag
//	requires_unlim=true|false      filter by RequiresUnlim flag
//	q=<substring>                  case-insensitive alias substring match
//	include_unstable=1             include Unstable specs (excluded by default)
//	include_deprecated=1           include Deprecated specs (excluded by default)
//	limit=<int>                    page size (default 100, cap 500)
//	offset=<int>                   page start
//
// Tier gating (PlanGate) is not modelled on ModelSpec, so a ?tier= filter
// is intentionally not exposed. Callers wanting a paid-only view should
// use requires_paid=true.
func (h *Handler) HandleModelsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Registry-level filters: Output and the Unstable/Deprecated gates.
	regFilter := ports.ModelFilter{
		Output:            q.Get("output"),
		IncludeUnstable:   q.Get("include_unstable") == "1",
		IncludeDeprecated: q.Get("include_deprecated") == "1",
	}

	// Parse handler-level flag filters. Empty string means "no filter".
	requiresPaid, ok := parseOptionalBool(w, q.Get("requires_paid"), "requires_paid")
	if !ok {
		return
	}
	requiresUnlim, ok := parseOptionalBool(w, q.Get("requires_unlim"), "requires_unlim")
	if !ok {
		return
	}

	limit, offset, ok := parseModelsPaging(w, q)
	if !ok {
		return
	}

	qSubstr := strings.ToLower(strings.TrimSpace(q.Get("q")))

	models := h.Registry.List(regFilter)

	// Apply handler-level filters that the registry does not know about.
	filtered := make([]*domain.ModelSpec, 0, len(models))
	for _, m := range models {
		if requiresPaid != nil && m.RequiresPaid != *requiresPaid {
			continue
		}
		if requiresUnlim != nil && m.RequiresUnlim != *requiresUnlim {
			continue
		}
		if qSubstr != "" && !strings.Contains(strings.ToLower(m.Alias), qSubstr) {
			continue
		}
		filtered = append(filtered, m)
	}

	totalBefore := len(filtered)

	// Apply pagination. Offsets past the end yield an empty page rather
	// than a 400 so clients can iterate blindly until data is empty.
	start := offset
	if start > totalBefore {
		start = totalBefore
	}
	end := start + limit
	if end > totalBefore {
		end = totalBefore
	}
	page := filtered[start:end]

	data := make([]map[string]any, 0, len(page))
	for _, m := range page {
		data = append(data, modelView(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":                  "list",
		"data":                    data,
		"limit":                   limit,
		"offset":                  offset,
		"total_before_pagination": totalBefore,
	})
}

// modelView renders a ModelSpec as the map shape emitted by
// GET /v1/models. Kept close to the handler so the wire contract stays
// visible next to the endpoint that promises it.
func modelView(m *domain.ModelSpec) map[string]any {
	return map[string]any{
		"id":              m.Alias,
		"object":          "model",
		"output":          m.Output,
		"jst":             m.JST,
		"est_cost":        float64(m.EstCostHundredths) / 100.0,
		"required_params": m.RequiredParams,
		"unstable":        m.Unstable,
		"requires_paid":   m.RequiresPaid,
		"requires_unlim":  m.RequiresUnlim,
		"requires_ultra":  m.RequiresUltra,
		"starter_locked":  m.StarterLocked,
	}
}

// parseOptionalBool reads a tri-state query flag: absent (returns nil,
// true), "true"/"1" (returns *true, true), "false"/"0" (returns *false,
// true), anything else writes a 400 and returns (nil, false).
func parseOptionalBool(w http.ResponseWriter, raw, name string) (*bool, bool) {
	if raw == "" {
		return nil, true
	}
	switch strings.ToLower(raw) {
	case "true", "1":
		t := true
		return &t, true
	case "false", "0":
		f := false
		return &f, true
	default:
		writeError(w, http.StatusBadRequest, "invalid_query", name+" must be true or false")
		return nil, false
	}
}

// parseModelsPaging normalises limit/offset the same way the /v1/jobs
// endpoint does: default limit 100, cap 500, non-negative offset,
// invalid values render a 400.
func parseModelsPaging(w http.ResponseWriter, q map[string][]string) (int, int, bool) {
	limit := defaultModelsListLimit
	offset := 0
	if raws, ok := q["limit"]; ok && len(raws) > 0 && raws[0] != "" {
		v, err := strconv.Atoi(raws[0])
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be a non-negative integer")
			return 0, 0, false
		}
		if v == 0 {
			v = defaultModelsListLimit
		}
		if v > maxModelsListLimit {
			v = maxModelsListLimit
		}
		limit = v
	}
	if raws, ok := q["offset"]; ok && len(raws) > 0 && raws[0] != "" {
		v, err := strconv.Atoi(raws[0])
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid_query", "offset must be a non-negative integer")
			return 0, 0, false
		}
		offset = v
	}
	return limit, offset, true
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
// request. Resolution proceeds in tiers and short-circuits on the first
// match:
//
//  1. An explicit `group_id` in the request body wins outright — the
//     canonical way for a caller to disambiguate a multi-binding key.
//  2. Direct 1:1 binding on api_keys.group_id (migration 005): when the
//     caller's APIKey row carries a non-empty GroupID, it is used
//     verbatim and the M:N table is not consulted. This is the fast
//     path for the common CPA case (one partner key → one pool group).
//  3. Fall back to the M:N apikey_group_bindings table via GroupStore:
//     - zero bindings: returns "" and no error (default group scope).
//     - one binding:   returns that group's ID.
//     - multiple:      returns an ambiguous_group httpError so the
//     caller disambiguates by setting group_id explicitly.
//
// GroupStore errors are treated as best-effort failures: they are logged
// (when logger is non-nil) but not surfaced to the caller — the request
// continues with an empty group scope so a transient DB blip cannot
// black-hole every generation request.
//
// apiKey may be nil; that skips both tier 2 and tier 3.
func resolveGroup(ctx context.Context, groups ports.GroupStore, logger *slog.Logger, apiKey *domain.APIKey, requested string) (string, *httpError) {
	if requested != "" {
		return requested, nil
	}
	// Tier 2: direct 1:1 binding wins over the M:N table.
	if apiKey != nil && apiKey.GroupID != "" {
		return apiKey.GroupID, nil
	}
	// Tier 3: fall back to the M:N binding table.
	if groups == nil || apiKey == nil || apiKey.ID == "" {
		return "", nil
	}
	bound, err := groups.ListGroupsForAPIKey(ctx, apiKey.ID)
	if err != nil {
		if logger != nil {
			logger.Warn("group resolve failed",
				slog.String("api_key_id", apiKey.ID),
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
