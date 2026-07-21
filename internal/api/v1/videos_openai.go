package v1

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/domain"
)

// Sora-compatible video surface. See docs/OPENAI-VIDEO-COMPAT.md.
//
// The three routes below (POST /v1/videos, GET /v1/videos/{id},
// GET /v1/videos/{id}/content) mimic OpenAI's video shape closely
// enough that new-api / OneAPI's built-in Sora TaskAdaptor can drive
// higgsgo unmodified. Legacy /v1/video[s]/generations stay untouched.

const (
	soraObjectVideo  = "video"
	maxSoraJSONBody  = 1 << 20  // 1 MiB
	maxSoraMultipart = 32 << 20 // 32 MiB total multipart
	contentStreamBuf = 32 << 10 // 32 KiB streaming buffer per §8
)

// SoraMediaUploader is the pluggable seam that handles the
// data-URI / multipart-file input_reference path. When the handler
// receives raw bytes, it delegates to this uploader which is expected
// to reserve + PUT + commit against higgsfield's media store and
// return the resulting media_id.
//
// Wired at boot in main.go so this file stays free of an upstream /
// account-store dependency. Nil is the "no uploader wired" default;
// callers who try to send a data URI or file part in that build hit
// a 501 error.
type SoraMediaUploader interface {
	UploadImage(ctx context.Context, contentType string, body []byte) (mediaID string, err error)
}

// ContentProxy is the pluggable seam that fetches j.ResultURL and
// streams it back to the caller. Split off the handler so tests can
// substitute a deterministic fake without spinning up a real CDN.
type ContentProxy func(w http.ResponseWriter, r *http.Request, resultURL string)

// soraRequest is the parsed body carried through both the JSON and
// multipart entry points.
type soraRequest struct {
	Model       string
	Prompt      string
	Seconds     int
	Width       int
	Height      int
	ImageURL    string // http(s) input_reference; forwarded to higgsfield as image_url
	MediaID     string // set after uploader.UploadImage returns for data-URI / file uploads
	GroupID     string
	Async       bool
	AsyncSet    bool // caller explicitly set async
	CallbackURL string
	Extra       map[string]any // extra_body passthrough
}

// HandleSoraVideoCreate serves POST /v1/videos. Accepts either
// application/json or multipart/form-data; the latter allows an
// optional input_reference file part to be uploaded to higgsfield's
// media store before the job is created.
func (h *Handler) HandleSoraVideoCreate(w http.ResponseWriter, r *http.Request) {
	sr, err := h.parseSoraRequest(r)
	if err != nil {
		writeSoraError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if sr.Model == "" {
		writeSoraError(w, http.StatusBadRequest, "invalid_body", "model is required")
		return
	}

	// Body conversion (Sora → higgsgo native). See §4.2.
	userParams := make(map[string]any)
	if sr.Prompt != "" {
		userParams["prompt"] = sr.Prompt
	}
	if sr.Seconds > 0 {
		userParams["duration"] = sr.Seconds
	}
	if sr.Width > 0 && sr.Height > 0 {
		userParams["width"] = sr.Width
		userParams["height"] = sr.Height
		userParams["resolution"] = resolutionTierFor(sr.Width, sr.Height)
	}
	// HTTP(S) URL variant of input_reference: forward to higgsfield as
	// image_url so upstream fetches the URL itself, matching the
	// legacy /v1/video/generations path.
	if sr.ImageURL != "" {
		userParams["image_url"] = sr.ImageURL
	}
	// extra_body / passthrough keys applied last so caller-supplied
	// values shadow anything the conversion layer wrote — but we
	// deliberately skip re-writing size/seconds/input_reference/media_id
	// keys because they were consumed above.
	for k, v := range sr.Extra {
		userParams[k] = v
	}

	// Resolve group, mirror videos.go.
	var (
		apiKey   *domain.APIKey
		apiKeyID string
	)
	if key, ok := middleware.APIKeyFromContext(r.Context()); ok {
		apiKey = key
		apiKeyID = key.ID
	}
	groupCandidates, herr := resolveGroup(r.Context(), h.Groups, h.Logger, apiKey, sr.GroupID)
	if herr != nil {
		writeSoraError(w, herr.Status, herr.Kind, herr.Message)
		return
	}
	primaryGroup := groupCandidates[0]

	greq := proxy.GenerationRequest{
		Model:           sr.Model,
		UserParams:      userParams,
		Async:           sr.Async,
		SyncRequested:   sr.AsyncSet,
		CallbackURL:     sr.CallbackURL,
		GroupID:         primaryGroup,
		GroupCandidates: groupCandidates,
		APIKeyID:        apiKeyID,
	}
	if apiKey != nil {
		greq.APIKeyMonthlyQuota = apiKey.MonthlyQuota
		greq.APIKeyMonthlyUsed = apiKey.MonthlyUsed
	}
	if sr.MediaID != "" {
		greq.Media = &proxy.MediaInput{PreUploadedID: sr.MediaID, Type: "image"}
	}

	resp, err := h.Service.Generate(r.Context(), greq)
	if err != nil {
		writeSoraGenerateError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, soraViewFromProxyResp(resp, sr.Seconds, sr.Width, sr.Height))
}

// HandleSoraVideoGet serves GET /v1/videos/{id} — the OpenAI-shaped
// poll endpoint. Returns the same envelope as create.
func (h *Handler) HandleSoraVideoGet(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeSoraError(w, http.StatusBadRequest, "invalid_id", "video id is required")
		return
	}
	if h.Jobs == nil {
		writeSoraError(w, http.StatusServiceUnavailable, "jobs_store_unavailable",
			"job persistence is not configured on this deployment")
		return
	}
	j, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrJobNotFound) {
			writeSoraError(w, http.StatusNotFound, "not_found", "video not found")
			return
		}
		writeSoraError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !soraCallerOwns(r.Context(), j) {
		writeSoraError(w, http.StatusNotFound, "not_found", "video not found")
		return
	}
	writeJSON(w, http.StatusOK, soraViewFromJob(j))
}

// HandleSoraVideoContent serves GET /v1/videos/{id}/content — reverse
// proxy for the completed MP4. Range headers pass through so clients
// can seek.
func (h *Handler) HandleSoraVideoContent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeSoraError(w, http.StatusBadRequest, "invalid_id", "video id is required")
		return
	}
	if h.Jobs == nil {
		writeSoraError(w, http.StatusServiceUnavailable, "jobs_store_unavailable",
			"job persistence is not configured on this deployment")
		return
	}
	j, err := h.Jobs.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrJobNotFound) {
			writeSoraError(w, http.StatusNotFound, "not_found", "video not found")
			return
		}
		writeSoraError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !soraCallerOwns(r.Context(), j) {
		writeSoraError(w, http.StatusNotFound, "not_found", "video not found")
		return
	}
	if j.Status != domain.JobCompleted {
		writeSoraError(w, http.StatusConflict, "not_ready", "video is not yet completed")
		return
	}
	if j.ResultURL == "" {
		writeSoraError(w, http.StatusNotFound, "not_found", "video content unavailable")
		return
	}
	proxier := h.ContentProxy
	if proxier == nil {
		proxier = defaultContentProxy
	}
	proxier(w, r, j.ResultURL)
}

// defaultContentProxy issues a server-side GET (no auth per §6) and
// streams the body back byte-for-byte. Only the caller's Range header
// is forwarded upstream so seek support works; Authorization / cookies
// are intentionally dropped to avoid leaking sk-hg- tokens to the CDN.
func defaultContentProxy(w http.ResponseWriter, r *http.Request, resultURL string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, resultURL, nil)
	if err != nil {
		writeSoraError(w, http.StatusBadGateway, "upstream_fetch_failed", err.Error())
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeSoraError(w, http.StatusBadGateway, "upstream_fetch_failed", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		writeSoraError(w, http.StatusBadGateway, "upstream_fetch_failed",
			fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode))
		return
	}
	for _, hdr := range []string{"Content-Type", "Content-Length", "Last-Modified", "ETag", "Accept-Ranges", "Content-Range"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, contentStreamBuf)
	_, _ = io.CopyBuffer(w, resp.Body, buf)
}

// parseSoraRequest reads the body, chooses the JSON or multipart parse
// path, and invokes the uploader when a data-URI or file part carries
// raw image bytes. Returned soraRequest is ready to feed into
// buildBody / proxy.Service.Generate.
func (h *Handler) parseSoraRequest(r *http.Request) (soraRequest, error) {
	ct := r.Header.Get("Content-Type")
	var (
		sr          soraRequest
		uploadCT    string
		uploadBytes []byte
		err         error
	)
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		sr, uploadCT, uploadBytes, err = parseSoraMultipart(r)
	default:
		sr, uploadCT, uploadBytes, err = parseSoraJSON(r)
	}
	if err != nil {
		return soraRequest{}, err
	}
	// Data-URI / multipart uploads: run the injected uploader so the
	// resulting media_id lands on sr.MediaID before the job runs.
	if len(uploadBytes) > 0 {
		if h.SoraUploader == nil {
			return soraRequest{}, fmt.Errorf("input_reference upload requires SoraUploader wiring on this deployment")
		}
		mediaID, uerr := h.SoraUploader.UploadImage(r.Context(), uploadCT, uploadBytes)
		if uerr != nil {
			return soraRequest{}, fmt.Errorf("upload input_reference: %w", uerr)
		}
		sr.MediaID = mediaID
	}
	return sr, nil
}

// parseSoraJSON decodes the JSON body. Data-URI input_reference values
// come back as raw bytes for the caller to upload; HTTP URLs stay in
// sr.ImageURL for higgsfield to fetch itself.
func parseSoraJSON(r *http.Request) (soraRequest, string, []byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxSoraJSONBody))
	if err != nil {
		return soraRequest{}, "", nil, err
	}
	_ = r.Body.Close()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return soraRequest{}, "", nil, err
	}
	sr := soraRequest{Extra: make(map[string]any)}
	if err := extractSoraKnownFields(top, &sr); err != nil {
		return soraRequest{}, "", nil, err
	}
	// Remaining keys → extra passthrough.
	for k, v := range top {
		var anyV any
		if err := json.Unmarshal(v, &anyV); err == nil {
			sr.Extra[k] = anyV
		}
	}
	// input_reference: HTTP URL passes through; data URI decodes to
	// raw bytes for the caller to upload.
	if ref := sr.ImageURL; ref != "" {
		switch {
		case isHTTPURL(ref):
			// Keep sr.ImageURL as-is — the handler forwards it to
			// higgsfield as image_url.
			return sr, "", nil, nil
		case strings.HasPrefix(ref, "data:"):
			ct, body, err := decodeDataURI(ref)
			if err != nil {
				return soraRequest{}, "", nil, fmt.Errorf("input_reference: %w", err)
			}
			sr.ImageURL = ""
			return sr, ct, body, nil
		default:
			return soraRequest{}, "", nil, fmt.Errorf("input_reference: must be http(s):// URL or data: URI")
		}
	}
	return sr, "", nil, nil
}

// parseSoraMultipart reads a multipart/form-data body. Every non-file
// field maps 1:1 to the JSON body; a file part named input_reference
// yields raw bytes that the caller uploads via SoraUploader.
func parseSoraMultipart(r *http.Request) (soraRequest, string, []byte, error) {
	if err := r.ParseMultipartForm(maxSoraMultipart); err != nil {
		return soraRequest{}, "", nil, err
	}
	sr := soraRequest{Extra: make(map[string]any)}
	for k, vs := range r.MultipartForm.Value {
		if len(vs) == 0 {
			continue
		}
		v := vs[0]
		switch k {
		case "model":
			sr.Model = v
		case "prompt":
			sr.Prompt = v
		case "seconds":
			if n, err := strconv.Atoi(v); err == nil {
				sr.Seconds = n
			} else {
				return soraRequest{}, "", nil, fmt.Errorf("seconds: %w", err)
			}
		case "size":
			wv, hv, ok := parseSize(v)
			if !ok {
				return soraRequest{}, "", nil, fmt.Errorf("size: expected WxH")
			}
			sr.Width, sr.Height = wv, hv
		case "input_reference":
			// String-form input_reference: URL or data URI, same as JSON.
			switch {
			case isHTTPURL(v):
				sr.ImageURL = v
			case strings.HasPrefix(v, "data:"):
				ct, body, err := decodeDataURI(v)
				if err != nil {
					return soraRequest{}, "", nil, fmt.Errorf("input_reference: %w", err)
				}
				return sr, ct, body, nil
			default:
				return soraRequest{}, "", nil, fmt.Errorf("input_reference: must be http(s):// URL or data: URI")
			}
		case "group_id":
			sr.GroupID = v
		case "async":
			sr.AsyncSet = true
			sr.Async = v == "true" || v == "1"
		case "callback_url":
			sr.CallbackURL = v
		default:
			sr.Extra[k] = v
		}
	}
	if files, ok := r.MultipartForm.File["input_reference"]; ok && len(files) > 0 {
		fh := files[0]
		f, err := fh.Open()
		if err != nil {
			return soraRequest{}, "", nil, fmt.Errorf("input_reference file: %w", err)
		}
		defer f.Close()
		buf, err := io.ReadAll(f)
		if err != nil {
			return soraRequest{}, "", nil, fmt.Errorf("input_reference file: %w", err)
		}
		ct := fh.Header.Get("Content-Type")
		if ct == "" {
			ct = "image/jpeg"
		}
		return sr, ct, buf, nil
	}
	return sr, "", nil, nil
}

// extractSoraKnownFields fills sr's typed slots from the top-level JSON
// map, deleting each consumed key so the remainder is extra_body.
func extractSoraKnownFields(top map[string]json.RawMessage, sr *soraRequest) error {
	if v, ok := top["model"]; ok {
		if err := json.Unmarshal(v, &sr.Model); err != nil {
			return fmt.Errorf("model: %w", err)
		}
		delete(top, "model")
	}
	if v, ok := top["prompt"]; ok {
		if err := json.Unmarshal(v, &sr.Prompt); err != nil {
			return fmt.Errorf("prompt: %w", err)
		}
		delete(top, "prompt")
	}
	if v, ok := top["seconds"]; ok {
		n, err := parseIntOrStringInt(v)
		if err != nil {
			return fmt.Errorf("seconds: %w", err)
		}
		sr.Seconds = n
		delete(top, "seconds")
	}
	if v, ok := top["size"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return fmt.Errorf("size: %w", err)
		}
		wv, hv, valid := parseSize(s)
		if !valid {
			return fmt.Errorf("size: expected WxH like 1280x720")
		}
		sr.Width, sr.Height = wv, hv
		delete(top, "size")
	}
	if v, ok := top["input_reference"]; ok {
		if err := json.Unmarshal(v, &sr.ImageURL); err != nil {
			return fmt.Errorf("input_reference: %w", err)
		}
		delete(top, "input_reference")
	}
	if v, ok := top["group_id"]; ok {
		if err := json.Unmarshal(v, &sr.GroupID); err != nil {
			return fmt.Errorf("group_id: %w", err)
		}
		delete(top, "group_id")
	}
	if v, ok := top["async"]; ok {
		if err := json.Unmarshal(v, &sr.Async); err != nil {
			return fmt.Errorf("async: %w", err)
		}
		sr.AsyncSet = true
		delete(top, "async")
	}
	if v, ok := top["callback_url"]; ok {
		if err := json.Unmarshal(v, &sr.CallbackURL); err != nil {
			return fmt.Errorf("callback_url: %w", err)
		}
		delete(top, "callback_url")
	}
	// §11.3: seconds wins over duration when both are set. If only
	// duration is present we still consume it into sr.Seconds so the
	// higgsgo-native form works via extra_body.
	if v, ok := top["duration"]; ok {
		if sr.Seconds == 0 {
			if n, err := parseIntOrStringInt(v); err == nil {
				sr.Seconds = n
			}
		}
		delete(top, "duration")
	}
	return nil
}

// parseIntOrStringInt accepts either a JSON number or a numeric JSON
// string. OpenAI SDKs serialize numeric duration fields as strings so
// both round-trips must work.
func parseIntOrStringInt(raw json.RawMessage) (int, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}

// decodeDataURI parses a "data:image/*;base64,..." URI and returns
// content type + raw bytes.
func decodeDataURI(s string) (string, []byte, error) {
	if !strings.HasPrefix(s, "data:") {
		return "", nil, fmt.Errorf("not a data URI")
	}
	rest := strings.TrimPrefix(s, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", nil, fmt.Errorf("no comma in data URI")
	}
	metadata := rest[:comma]
	payload := rest[comma+1:]
	parts := strings.Split(metadata, ";")
	contentType := parts[0]
	if contentType == "" {
		contentType = "image/jpeg"
	}
	base64Encoded := false
	for _, p := range parts[1:] {
		if p == "base64" {
			base64Encoded = true
			break
		}
	}
	if !base64Encoded {
		return "", nil, fmt.Errorf("only base64-encoded data URIs are supported")
	}
	body, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		body, err = base64.RawURLEncoding.DecodeString(payload)
		if err != nil {
			return "", nil, fmt.Errorf("base64: %w", err)
		}
	}
	return contentType, body, nil
}

// isHTTPURL is a cheap prefix check; net/url would over-engineer this.
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// resolutionTierFor picks the higgsfield-native `resolution` token
// from the SHORTER side of the request size. This matches Appendix A
// verbatim (§9 designates those two samples as the source-of-truth
// test inputs: 1280x720 → "720p"; 1024x1792 → "1080p") and standard
// video-industry practice where "720p" describes the vertical
// resolution in landscape or the minor dimension in portrait. The
// §4.2 text erroneously says "longer side"; the Appendix samples
// disagree and win. See videos_openai_test.go for the pinned mapping.
func resolutionTierFor(w, h int) string {
	shorter := w
	if h < shorter {
		shorter = h
	}
	switch {
	case shorter <= 480:
		return "480p"
	case shorter <= 720:
		return "720p"
	case shorter <= 1080:
		return "1080p"
	default:
		return "4k"
	}
}

// soraViewFromProxyResp renders the immediate create-time response in
// the Sora envelope. seconds / width / height come from the caller's
// request so the SDK round-trips them.
func soraViewFromProxyResp(resp *proxy.GenerationResponse, seconds, width, height int) map[string]any {
	out := map[string]any{
		"id":         resp.ID,
		"object":     soraObjectVideo,
		"model":      resp.Model,
		"status":     soraStatus(resp.Status),
		"progress":   soraProgressFor(resp.Status),
		"created_at": resp.CreatedAt,
	}
	if seconds > 0 {
		out["seconds"] = strconv.Itoa(seconds)
	}
	if width > 0 && height > 0 {
		out["size"] = fmt.Sprintf("%dx%d", width, height)
	}
	if soraStatus(resp.Status) == "completed" {
		out["completed_at"] = resp.CreatedAt
	}
	if resp.Error != nil {
		out["error"] = map[string]any{
			"message": resp.Error.Message,
			"code":    resp.Error.Type,
		}
	}
	return out
}

// soraViewFromJob renders the polled envelope from a persisted job.
// Deliberately excludes internal fields (poll_url, cost,
// upstream_job_id, result_url, error_detail) per §4.3.
func soraViewFromJob(j *domain.Job) map[string]any {
	status := soraStatus(string(j.Status))
	out := map[string]any{
		"id":         j.ID,
		"object":     soraObjectVideo,
		"model":      j.ModelAlias,
		"status":     status,
		"progress":   soraProgressFor(string(j.Status)),
		"created_at": j.RequestTS.Unix(),
	}
	// Echo seconds / size back from the stored request body when we can.
	if j.RequestBodyJSON != "" {
		var body struct {
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal([]byte(j.RequestBodyJSON), &body); err == nil && body.Params != nil {
			if d, ok := body.Params["duration"]; ok {
				out["seconds"] = fmt.Sprintf("%v", d)
			}
			if wv, ok := body.Params["width"].(float64); ok {
				if hv, ok := body.Params["height"].(float64); ok && wv > 0 && hv > 0 {
					out["size"] = fmt.Sprintf("%dx%d", int(wv), int(hv))
				}
			}
		}
	}
	if !j.FinishedAt.IsZero() && status == "completed" {
		out["completed_at"] = j.FinishedAt.Unix()
	}
	if j.ErrorType != "" || j.ErrorDetail != "" {
		msg := j.ErrorDetail
		if msg == "" {
			msg = string(j.ErrorType)
		}
		out["error"] = map[string]any{
			"message": msg,
			"code":    string(j.ErrorType),
		}
	}
	return out
}

// soraStatus maps higgsgo's JobStatus values to the four Sora status
// literals. `pending` and `refunded` surface as `queued` / `failed`
// respectively per §7.
func soraStatus(s string) string {
	switch domain.JobStatus(s) {
	case domain.JobPending, domain.JobQueued:
		return "queued"
	case domain.JobRunning:
		return "in_progress"
	case domain.JobCompleted:
		return "completed"
	case domain.JobFailed, domain.JobRefunded, domain.JobTimeout:
		return "failed"
	}
	// Also handle raw proxy strings without the domain-typed wrapper.
	switch s {
	case "queued", "in_progress", "completed", "failed":
		return s
	}
	return "queued"
}

// soraProgressFor picks a coarse percent from the state machine. Real
// per-poll progress is not tracked upstream so 0 / 50 / 100 is the
// most the Sora clients need.
func soraProgressFor(status string) int {
	switch soraStatus(status) {
	case "in_progress":
		return 50
	case "completed":
		return 100
	}
	return 0
}

// soraCallerOwns enforces per-key visibility. The public /v1
// middleware always attaches an APIKey; the negative branch is a
// wiring bug.
func soraCallerOwns(ctx context.Context, j *domain.Job) bool {
	key, ok := middleware.APIKeyFromContext(ctx)
	if !ok || key == nil {
		return false
	}
	return j.APIKeyID == "" || j.APIKeyID == key.ID
}

// writeSoraError writes the OpenAI-shaped error envelope per §4.4.
// Distinct from writeError so the Sora surface's wire shape stays
// stable — top-level `error.message` + `error.code` — even if
// higgsgo's native handlers ever evolve theirs.
func writeSoraError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"code":    code,
		},
	})
}

// writeSoraGenerateError renders a proxy.Service.Generate error into
// the Sora envelope. Mirrors writeGenerationError but uses the Sora
// shape.
func writeSoraGenerateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrModelNotFound):
		writeSoraError(w, http.StatusNotFound, "model_not_found", err.Error())
	case errors.Is(err, domain.ErrNoEligibleAccount):
		writeSoraError(w, http.StatusServiceUnavailable, "no_capacity", err.Error())
	case errors.Is(err, domain.ErrGroupConcurrencyMax):
		writeSoraError(w, http.StatusTooManyRequests, "pool_saturated", err.Error())
	case errors.Is(err, domain.ErrModelBlocked):
		writeSoraError(w, http.StatusForbidden, "model_blocked", err.Error())
	case errors.Is(err, domain.ErrModelNotAllowed):
		writeSoraError(w, http.StatusForbidden, "model_not_allowed", err.Error())
	case errors.Is(err, domain.ErrGroupQuotaExhausted):
		writeSoraError(w, http.StatusPaymentRequired, "group_budget_exhausted", err.Error())
	case errors.Is(err, domain.ErrAPIKeyQuotaExceed):
		writeSoraError(w, http.StatusPaymentRequired, "quota_exhausted", err.Error())
	case errors.Is(err, domain.ErrUpstreamForbidden):
		writeSoraError(w, http.StatusPaymentRequired, "plan_gate", err.Error())
	case errors.Is(err, domain.ErrUpstreamRateLimit):
		writeSoraError(w, http.StatusTooManyRequests, "rate_limit", err.Error())
	case errors.Is(err, domain.ErrUpstreamBadBody):
		writeSoraError(w, http.StatusBadRequest, "body_error", err.Error())
	case errors.Is(err, domain.ErrUpstreamTimeout):
		writeSoraError(w, http.StatusGatewayTimeout, "upstream_timeout", err.Error())
	case errors.Is(err, domain.ErrUpstreamUnauthorized):
		writeSoraError(w, http.StatusUnauthorized, "upstream_auth", err.Error())
	case errors.Is(err, domain.ErrUpstreamServerError):
		writeSoraError(w, http.StatusBadGateway, "upstream_5xx", err.Error())
	default:
		writeSoraError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
