package cpaplugin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// executeRequest is the body shape for POST /internal/execute. It mirrors
// the /v1 handler payload but is scoped to a CPA partner: the upstream
// platform tells us which of its partner-owned api keys should be billed
// for the run.
type executeRequest struct {
	APIKeyID     string         `json:"api_key_id,omitempty"`
	CPAPartnerID string         `json:"cpa_partner_id,omitempty"`
	Model        string         `json:"model"`
	Prompt       string         `json:"prompt,omitempty"`
	Async        bool           `json:"async,omitempty"`
	SyncFlagSet  bool           `json:"-"`
	CallbackURL  string         `json:"callback_url,omitempty"`
	GroupID      string         `json:"group_id,omitempty"`
	ImageURL     string         `json:"image_url,omitempty"`
	VideoURL     string         `json:"video_url,omitempty"`
	MediaID      string         `json:"media_id,omitempty"`
	Params       map[string]any `json:"params,omitempty"`
}

// HandleExecute proxies a CPA-side user request through the higgsgo pool.
//
// Resolution order for the api key to charge:
//  1. body.api_key_id, if given (verified to exist).
//  2. otherwise the first active api_keys row belonging to body.cpa_partner_id.
//
// The request body may include free-form "params" that get forwarded to
// the upstream model — this matches the /v1 handler's Extra map behaviour.
func (h *Handler) HandleExecute(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_body", "empty body")
		return
	}
	var req executeRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	// Track whether the caller explicitly set async so the service can fall
	// back to its AsyncByDefault heuristic when they did not.
	var rawFields map[string]any
	if err := json.Unmarshal(raw, &rawFields); err == nil {
		_, req.SyncFlagSet = rawFields["async"]
	}
	if req.Model == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "model is required")
		return
	}
	if req.APIKeyID == "" && req.CPAPartnerID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_body", "api_key_id or cpa_partner_id is required")
		return
	}

	apiKeyID, herr := h.resolveExecuteKey(r, &req)
	if herr != nil {
		writeErr(w, herr.status, herr.kind, herr.msg)
		return
	}

	userParams := make(map[string]any)
	if req.Prompt != "" {
		userParams["prompt"] = req.Prompt
	}
	for k, v := range req.Params {
		userParams[k] = v
	}

	greq := proxy.GenerationRequest{
		Model:         req.Model,
		UserParams:    userParams,
		Async:         req.Async,
		SyncRequested: req.SyncFlagSet,
		CallbackURL:   req.CallbackURL,
		GroupID:       req.GroupID,
		APIKeyID:      apiKeyID,
		CPAPartnerID:  req.CPAPartnerID,
	}
	if req.MediaID != "" || req.ImageURL != "" || req.VideoURL != "" {
		mt := "image"
		url := req.ImageURL
		if req.VideoURL != "" {
			mt = "video"
			url = req.VideoURL
		}
		greq.Media = &proxy.MediaInput{
			PreUploadedID: req.MediaID,
			Type:          mt,
			URL:           url,
		}
	}

	resp, err := h.Proxy.Generate(r.Context(), greq)
	if err != nil {
		if errors.Is(err, domain.ErrModelNotFound) {
			writeErr(w, http.StatusNotFound, "model_not_found", err.Error())
			return
		}
		writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// httpErr is a small internal error carrier used to keep the resolution
// helper below readable. Not exported.
type httpErr struct {
	status int
	kind   string
	msg    string
}

// resolveExecuteKey figures out which api_keys row should be charged for
// this execute call. See HandleExecute for the resolution order.
func (h *Handler) resolveExecuteKey(r *http.Request, req *executeRequest) (string, *httpErr) {
	if req.APIKeyID != "" {
		k, err := h.APIKeys.Get(r.Context(), req.APIKeyID)
		if err != nil {
			if errors.Is(err, domain.ErrAPIKeyNotFound) {
				return "", &httpErr{http.StatusNotFound, "api_key_not_found", "api_key_id does not exist"}
			}
			return "", &httpErr{http.StatusInternalServerError, "internal", err.Error()}
		}
		if k.Status != "active" {
			return "", &httpErr{http.StatusForbidden, "api_key_revoked", "api_key_id is not active"}
		}
		return k.ID, nil
	}
	keys, err := h.listKeysForPartner(r.Context(), req.CPAPartnerID)
	if err != nil {
		return "", &httpErr{http.StatusInternalServerError, "internal", err.Error()}
	}
	for i := range keys {
		if keys[i].Status == "active" {
			return keys[i].ID, nil
		}
	}
	return "", &httpErr{http.StatusNotFound, "partner_not_registered", "no active api key for cpa_partner_id"}
}
