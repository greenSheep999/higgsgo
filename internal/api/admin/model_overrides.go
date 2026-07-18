package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// ModelOverridesHandler serves /admin/models/overrides and the per-
// alias /admin/models/{alias}/override surface. Backed by
// ports.ModelOverrideStore (migration 015) and coupled to a
// ports.ModelRegistry so a write can trigger a reload of the merged
// snapshot — otherwise the very next /v1/models call would still
// serve the pre-write view.
type ModelOverridesHandler struct {
	Store    ports.ModelOverrideStore
	Registry ports.ModelRegistry
	Logger   *slog.Logger
}

// NewModelOverridesHandler wires the handler. Store must be non-nil
// (the mount is gated at the server level). Registry may be nil in
// tests — writes will still succeed, only the in-memory refresh is
// skipped.
func NewModelOverridesHandler(store ports.ModelOverrideStore, reg ports.ModelRegistry, logger *slog.Logger) *ModelOverridesHandler {
	return &ModelOverridesHandler{Store: store, Registry: reg, Logger: logger}
}

// Register mounts the routes under /models. Kept adjacent to the
// existing /models/reload endpoint so operators only need one bearer
// for both surfaces.
func (h *ModelOverridesHandler) Register(r chi.Router) {
	r.Get("/models/overrides", h.List)
	r.Get("/models/{alias}/override", h.Get)
	r.Put("/models/{alias}/override", h.Put)
	r.Delete("/models/{alias}/override", h.Delete)
}

// List returns every override row, newest updated first.
func (h *ModelOverridesHandler) List(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "model overrides store not configured")
		return
	}
	rows, err := h.Store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, o := range rows {
		out = append(out, overrideView(&o))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(out),
		"data":  out,
	})
}

// Get returns a single override, or 404 when none exists for alias.
func (h *ModelOverridesHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "model overrides store not configured")
		return
	}
	alias := strings.TrimSpace(chi.URLParam(r, "alias"))
	if alias == "" {
		writeErr(w, http.StatusBadRequest, "invalid_alias", "alias is required")
		return
	}
	o, err := h.Store.Get(r.Context(), alias)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if o == nil {
		writeErr(w, http.StatusNotFound, "not_found", "no override for alias")
		return
	}
	writeJSON(w, http.StatusOK, overrideView(o))
}

// overridePatch is the wire body accepted by PUT
// /admin/models/{alias}/override. Every tier field is a `*bool` so
// the caller can omit it (leave unchanged), set null (unset override
// → inherit spec), or set true/false (explicit override). ExtraAliases
// is always taken verbatim from the body — passing an empty array
// clears the expansion.
type overridePatch struct {
	StarterLocked        *bool     `json:"starter_locked"`
	RequiresPaid         *bool     `json:"requires_paid"`
	RequiresUltra        *bool     `json:"requires_ultra"`
	RequiresUnlim        *bool     `json:"requires_unlim"`
	MinCreditsHundredths *int64    `json:"min_credits_hundredths"`
	ExtraAliases         *[]string `json:"extra_aliases"`
	Note                 *string   `json:"note"`
}

// Put upserts the override row for {alias}. The body is a partial
// patch — fields that arrive nil are treated as "unset" (or "leave as
// the previous stored value"). The current row (if any) is fetched
// so the merge doesn't blow away an unrelated flag.
func (h *ModelOverridesHandler) Put(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "model overrides store not configured")
		return
	}
	alias := strings.TrimSpace(chi.URLParam(r, "alias"))
	if alias == "" {
		writeErr(w, http.StatusBadRequest, "invalid_alias", "alias is required")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var patch overridePatch
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &patch); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
			return
		}
	}
	// Reject an alias whose current registry Resolve() 404s so an
	// operator can't accidentally overwrite an override for a since-
	// removed catalog entry (the row would still land in SQLite but
	// never merge into anything on /v1/models).
	if h.Registry != nil {
		if _, err := h.Registry.Resolve(alias); err != nil {
			if errors.Is(err, domain.ErrModelNotFound) {
				writeErr(w, http.StatusNotFound, "model_not_found",
					"alias not in the model catalog")
				return
			}
			writeErr(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}

	o := &domain.ModelOverride{
		Alias:                alias,
		StarterLocked:        patch.StarterLocked,
		RequiresPaid:         patch.RequiresPaid,
		RequiresUltra:        patch.RequiresUltra,
		RequiresUnlim:        patch.RequiresUnlim,
		MinCreditsHundredths: patch.MinCreditsHundredths,
	}
	if patch.ExtraAliases != nil {
		// De-dup while preserving order so an accidental repeated tag
		// entry doesn't propagate to the /v1/models response.
		o.ExtraAliases = dedupStrings(*patch.ExtraAliases)
	}
	if patch.Note != nil {
		o.Note = strings.TrimSpace(*patch.Note)
	}
	if err := h.Store.Upsert(r.Context(), o); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	h.refreshRegistry(r.Context())

	// Refetch so the response echoes the stored `updated_at`
	// timestamp — cheap and keeps the client cache aligned.
	stored, err := h.Store.Get(r.Context(), alias)
	if err != nil || stored == nil {
		writeJSON(w, http.StatusOK, overrideView(o))
		return
	}
	writeJSON(w, http.StatusOK, overrideView(stored))
}

// Delete clears the override for {alias}. Missing rows are 204s so
// the caller can idempotently reset without inspecting the status.
func (h *ModelOverridesHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "model overrides store not configured")
		return
	}
	alias := strings.TrimSpace(chi.URLParam(r, "alias"))
	if alias == "" {
		writeErr(w, http.StatusBadRequest, "invalid_alias", "alias is required")
		return
	}
	if err := h.Store.Delete(r.Context(), alias); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	h.refreshRegistry(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// refreshRegistry triggers a registry reload so the in-memory merged
// view lines up with the write we just made. A reload error is
// logged but not surfaced to the operator — the DB write succeeded,
// and the next successful reload (or restart) will pick it up.
func (h *ModelOverridesHandler) refreshRegistry(ctx context.Context) {
	if h.Registry == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := h.Registry.Reload(rctx); err != nil && h.Logger != nil {
		h.Logger.Warn("model override write: registry reload failed",
			slog.String("err", err.Error()))
	}
}

// overrideView renders one row for the wire. Nulls are preserved so
// the WebUI can distinguish "inherit" (JSON null) from "explicit
// false" (JSON false). ExtraAliases is always an array (never null)
// so the client renders a chip container without branching.
func overrideView(o *domain.ModelOverride) map[string]any {
	extra := o.ExtraAliases
	if extra == nil {
		extra = []string{}
	}
	m := map[string]any{
		"alias":                  o.Alias,
		"starter_locked":         boolPtrOrNil(o.StarterLocked),
		"requires_paid":          boolPtrOrNil(o.RequiresPaid),
		"requires_ultra":         boolPtrOrNil(o.RequiresUltra),
		"requires_unlim":         boolPtrOrNil(o.RequiresUnlim),
		"min_credits_hundredths": int64PtrOrNil(o.MinCreditsHundredths),
		"extra_aliases":          extra,
		"note":                   o.Note,
	}
	if !o.UpdatedAt.IsZero() {
		m["updated_at"] = o.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return m
}

func boolPtrOrNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func int64PtrOrNil(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// dedupStrings preserves order but drops empty / whitespace-only
// entries plus duplicates. Deliberately not exported — used only by
// this handler.
func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
