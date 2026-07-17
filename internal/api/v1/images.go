package v1

import (
	"encoding/json"
	"net/http"

	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
)

// imageRequest mirrors OpenAI's POST /v1/images/generations shape with a
// few passthrough keys we forward to higgsfield's params.
type imageRequest struct {
	Model       string `json:"model"`
	Prompt      string `json:"prompt"`
	N           int    `json:"n,omitempty"`
	Size        string `json:"size,omitempty"`     // "1024x1024"
	Quality     string `json:"quality,omitempty"`  // "standard" | "hd"
	ImageID     string `json:"media_id,omitempty"` // pre-uploaded higgsfield media
	Async       bool   `json:"async,omitempty"`
	CallbackURL string `json:"callback_url,omitempty"`
	// GroupID, when set, restricts the pool pick to a specific account
	// group. When empty, the handler auto-resolves the group from the
	// caller's api key bindings via resolveGroup.
	GroupID string `json:"group_id,omitempty"`
}

// HandleImageGeneration serves POST /v1/images/generations.
func (h *Handler) HandleImageGeneration(w http.ResponseWriter, r *http.Request) {
	raw, err := readAll(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	var ir imageRequest
	if err := json.Unmarshal(raw, &ir); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if ir.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_body", "model is required")
		return
	}
	// Forward unknown keys.
	var extraMap map[string]any
	_ = json.Unmarshal(raw, &extraMap)
	known := map[string]bool{"model": true, "prompt": true, "n": true, "size": true, "quality": true, "media_id": true, "async": true, "callback_url": true, "group_id": true}
	userParams := make(map[string]any)
	if ir.Prompt != "" {
		userParams["prompt"] = ir.Prompt
	}
	if ir.N > 0 {
		userParams["batch_size"] = ir.N
	}
	if ir.Size != "" {
		// OpenAI-style "1024x1024" → higgsfield params.width/height.
		w, h, ok := parseSize(ir.Size)
		if ok {
			userParams["width"] = w
			userParams["height"] = h
		}
	}
	for k, v := range extraMap {
		if !known[k] {
			userParams[k] = v
		}
	}

	_, syncRequested := extraMap["async"]

	// Resolve which group scopes the pool pick. When the caller sets
	// group_id we honour it; otherwise the api key's bindings are queried
	// via GroupStore.
	var apiKeyID string
	if key, ok := middleware.APIKeyFromContext(r.Context()); ok {
		apiKeyID = key.ID
	}
	groupID, herr := resolveGroup(r.Context(), h.Groups, h.Logger, apiKeyID, ir.GroupID)
	if herr != nil {
		writeError(w, herr.Status, herr.Kind, herr.Message)
		return
	}

	greq := proxy.GenerationRequest{
		Model:         ir.Model,
		UserParams:    userParams,
		Async:         ir.Async,
		SyncRequested: syncRequested,
		CallbackURL:   ir.CallbackURL,
		GroupID:       groupID,
		APIKeyID:      apiKeyID,
	}
	if ir.ImageID != "" {
		greq.Media = &proxy.MediaInput{PreUploadedID: ir.ImageID, Type: "image"}
	}

	resp, err := h.Service.Generate(r.Context(), greq)
	if err != nil {
		writeGenerationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseSize accepts "WIDTHxHEIGHT" and returns the two ints.
func parseSize(s string) (int, int, bool) {
	var w, h int
	_, err := fmtSscanf(s, "%dx%d", &w, &h)
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}
