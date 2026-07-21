package v1

// Tests for the Sora-compatible video surface (POST /v1/videos,
// GET /v1/videos/{id}, GET /v1/videos/{id}/content). See
// docs/OPENAI-VIDEO-COMPAT.md.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// --- helpers ------------------------------------------------------------

// fakeSoraUploader records the last UploadImage call and returns a
// deterministic media_id so tests can assert the wiring.
type fakeSoraUploader struct {
	lastContentType string
	lastBytes       []byte
	mediaID         string
	err             error
}

func (f *fakeSoraUploader) UploadImage(_ context.Context, ct string, body []byte) (string, error) {
	f.lastContentType = ct
	f.lastBytes = append([]byte(nil), body...)
	if f.err != nil {
		return "", f.err
	}
	if f.mediaID == "" {
		return "media_test", nil
	}
	return f.mediaID, nil
}

// fakeSoraJobStore serves the poll + content endpoints. Only Get and
// Create are exercised; other methods panic to keep the tests honest.
type fakeSoraJobStore struct {
	byID map[string]*domain.Job
	err  error
}

func (f *fakeSoraJobStore) Create(_ context.Context, j *domain.Job) error {
	if f.byID == nil {
		f.byID = make(map[string]*domain.Job)
	}
	f.byID[j.ID] = j
	return nil
}
func (f *fakeSoraJobStore) UpdateStatus(context.Context, string, domain.JobStatus, ports.JobMeta) error {
	panic("not implemented")
}
func (f *fakeSoraJobStore) TryMarkTerminal(context.Context, string, []domain.JobStatus, domain.JobStatus, ports.JobMeta) (bool, error) {
	panic("not implemented")
}
func (f *fakeSoraJobStore) Get(_ context.Context, id string) (*domain.Job, error) {
	if f.err != nil {
		return nil, f.err
	}
	j, ok := f.byID[id]
	if !ok {
		return nil, domain.ErrJobNotFound
	}
	return j, nil
}
func (f *fakeSoraJobStore) ListPending(context.Context) ([]domain.Job, error) {
	panic("not implemented")
}
func (f *fakeSoraJobStore) ListByAPIKey(context.Context, string, ports.JobFilter) ([]domain.Job, error) {
	panic("not implemented")
}
func (f *fakeSoraJobStore) ListAll(context.Context, ports.JobFilter) ([]domain.Job, error) {
	panic("not implemented")
}
func (f *fakeSoraJobStore) Purge(context.Context, time.Time, []domain.JobStatus) (int, error) {
	panic("not implemented")
}

// newSoraTestRouter wires the three Sora routes onto a chi router with
// an APIKey injected in ctx so soraCallerOwns has something to match.
func newSoraTestRouter(t *testing.T, h *Handler, keyID string) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := middleware.ContextWithAPIKey(req.Context(), &domain.APIKey{
				ID:     keyID,
				Status: "active",
			})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Post("/v1/videos", h.HandleSoraVideoCreate)
	r.Get("/v1/videos/{id}", h.HandleSoraVideoGet)
	r.Get("/v1/videos/{id}/content", h.HandleSoraVideoContent)
	return r
}

// --- parseSoraJSON / body conversion table-driven --------------------------

// TestParseSoraJSON_KlingSample covers Appendix A.1 verbatim: kling-3-turbo
// at 1280x720, 5 seconds. The converted internal UserParams must carry
// the exact width/height/duration/resolution combo the spec locks.
func TestParseSoraJSON_KlingSample(t *testing.T) {
	body := `{"model":"kling-3-turbo","prompt":"a cat playing piano","seconds":"5","size":"1280x720"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	sr, ct, buf, err := parseSoraJSON(req)
	if err != nil {
		t.Fatalf("parseSoraJSON: %v", err)
	}
	if ct != "" || buf != nil {
		t.Fatalf("no upload expected for this sample; got ct=%q bytes=%d", ct, len(buf))
	}
	if sr.Model != "kling-3-turbo" {
		t.Errorf("Model: got %q want kling-3-turbo", sr.Model)
	}
	if sr.Prompt != "a cat playing piano" {
		t.Errorf("Prompt: got %q", sr.Prompt)
	}
	if sr.Seconds != 5 {
		t.Errorf("Seconds: got %d want 5", sr.Seconds)
	}
	if sr.Width != 1280 || sr.Height != 720 {
		t.Errorf("Size: got %dx%d want 1280x720", sr.Width, sr.Height)
	}
	if r := resolutionTierFor(sr.Width, sr.Height); r != "720p" {
		t.Errorf("resolution: got %q want 720p", r)
	}
}

// TestParseSoraJSON_SoraSample covers Appendix A.2 verbatim:
// sora2-video portrait 1024x1792, 8 seconds. Appendix A.2 pins the
// expected resolution to "1080p"; that only holds when the tier is
// picked from the SHORTER side (1024 ≤ 1080). See resolutionTierFor
// for the rationale.
func TestParseSoraJSON_SoraSample(t *testing.T) {
	body := `{"model":"sora2-video","prompt":"a portrait test scene","seconds":"8","size":"1024x1792"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	sr, _, _, err := parseSoraJSON(req)
	if err != nil {
		t.Fatalf("parseSoraJSON: %v", err)
	}
	if sr.Width != 1024 || sr.Height != 1792 {
		t.Errorf("Size: got %dx%d want 1024x1792", sr.Width, sr.Height)
	}
	if sr.Seconds != 8 {
		t.Errorf("Seconds: got %d want 8", sr.Seconds)
	}
	if r := resolutionTierFor(sr.Width, sr.Height); r != "1080p" {
		t.Errorf("resolution: got %q want 1080p", r)
	}
}

// TestResolutionTierFor covers each edge of the locked tier table.
// Tier is picked from the shorter side; see resolutionTierFor for
// the rationale.
func TestResolutionTierFor(t *testing.T) {
	cases := []struct {
		name     string
		w, h     int
		wantTier string
	}{
		{"square_480", 480, 480, "480p"},
		{"landscape_360p_is_480", 640, 360, "480p"},
		{"landscape_720p", 1280, 720, "720p"},
		{"landscape_1080p", 1920, 1080, "1080p"},
		{"portrait_1024x1792", 1024, 1792, "1080p"},
		{"landscape_4k", 3840, 2160, "4k"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := resolutionTierFor(tc.w, tc.h)
			if got != tc.wantTier {
				t.Errorf("resolutionTierFor(%d,%d) = %q, want %q",
					tc.w, tc.h, got, tc.wantTier)
			}
		})
	}
}

// TestParseSoraJSON_SecondsAsInt covers the JSON-int variant. OpenAI's
// TypeScript SDK stringifies but callers writing curl by hand often
// send the raw int.
func TestParseSoraJSON_SecondsAsInt(t *testing.T) {
	body := `{"model":"m","seconds":5,"size":"1280x720"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	sr, _, _, err := parseSoraJSON(req)
	if err != nil {
		t.Fatalf("parseSoraJSON: %v", err)
	}
	if sr.Seconds != 5 {
		t.Errorf("Seconds: got %d want 5 (int variant)", sr.Seconds)
	}
}

// TestParseSoraJSON_ExtraBodyPassthrough asserts that keys the wire
// spec doesn't list — the OpenAI SDK's `extra_body` merge target —
// land in sr.Extra so the downstream buildBody picks them up as
// higgsfield-private params (mode / sound / generate_audio / …).
func TestParseSoraJSON_ExtraBodyPassthrough(t *testing.T) {
	body := `{"model":"m","seconds":5,"size":"1280x720","mode":"quality","sound":"on","generate_audio":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	sr, _, _, err := parseSoraJSON(req)
	if err != nil {
		t.Fatalf("parseSoraJSON: %v", err)
	}
	// Known-field slots must be filled.
	if sr.Model != "m" || sr.Seconds != 5 || sr.Width != 1280 || sr.Height != 720 {
		t.Fatalf("known slots not filled: %+v", sr)
	}
	// Extras must round-trip verbatim.
	if v, ok := sr.Extra["mode"].(string); !ok || v != "quality" {
		t.Errorf("Extra[mode]: got %v want quality", sr.Extra["mode"])
	}
	if v, ok := sr.Extra["sound"].(string); !ok || v != "on" {
		t.Errorf("Extra[sound]: got %v want on", sr.Extra["sound"])
	}
	if v, ok := sr.Extra["generate_audio"].(bool); !ok || !v {
		t.Errorf("Extra[generate_audio]: got %v want true", sr.Extra["generate_audio"])
	}
	// Known keys must NOT bleed into Extra.
	for _, k := range []string{"model", "prompt", "seconds", "size"} {
		if _, ok := sr.Extra[k]; ok {
			t.Errorf("known key %q leaked into Extra", k)
		}
	}
}

// TestParseSoraJSON_HTTPImageURLStaysAsURL: HTTP(S) input_reference
// values are forwarded to higgsfield as image_url — no upload.
func TestParseSoraJSON_HTTPImageURLStaysAsURL(t *testing.T) {
	body := `{"model":"m","input_reference":"https://cdn.example.com/a.jpg"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	sr, ct, buf, err := parseSoraJSON(req)
	if err != nil {
		t.Fatalf("parseSoraJSON: %v", err)
	}
	if sr.ImageURL != "https://cdn.example.com/a.jpg" {
		t.Errorf("ImageURL: got %q", sr.ImageURL)
	}
	if ct != "" || len(buf) != 0 {
		t.Errorf("expected no upload for HTTP URL, got ct=%q bytes=%d", ct, len(buf))
	}
}

// TestParseSoraJSON_DataURIDecoded: base64 data URIs decode to raw
// bytes and the URL slot is cleared so the handler routes them
// through the uploader instead of the image_url passthrough.
func TestParseSoraJSON_DataURIDecoded(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n_fake_png_bytes")
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	body := fmt.Sprintf(`{"model":"m","input_reference":%q}`, dataURI)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	sr, ct, buf, err := parseSoraJSON(req)
	if err != nil {
		t.Fatalf("parseSoraJSON: %v", err)
	}
	if sr.ImageURL != "" {
		t.Errorf("ImageURL: expected empty after data-URI decode, got %q", sr.ImageURL)
	}
	if ct != "image/png" {
		t.Errorf("content type: got %q want image/png", ct)
	}
	if !bytes.Equal(buf, png) {
		t.Errorf("decoded bytes mismatch: got %v want %v", buf, png)
	}
}

// TestParseSoraJSON_InvalidInputRef: neither URL nor data URI → 400.
func TestParseSoraJSON_InvalidInputRef(t *testing.T) {
	body := `{"model":"m","input_reference":"not-a-url"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	if _, _, _, err := parseSoraJSON(req); err == nil {
		t.Fatalf("expected error for invalid input_reference")
	}
}

// TestSecondsWinsOverDuration: §11.3 — when both are set, seconds wins.
func TestSecondsWinsOverDuration(t *testing.T) {
	body := `{"model":"m","seconds":"7","duration":99}`
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	sr, _, _, err := parseSoraJSON(req)
	if err != nil {
		t.Fatalf("parseSoraJSON: %v", err)
	}
	if sr.Seconds != 7 {
		t.Errorf("Seconds: got %d want 7 (Sora field wins)", sr.Seconds)
	}
}

// --- multipart parse -----------------------------------------------------

// TestParseSoraMultipart_FileUpload verifies a POST with a file part
// carrying raw image bytes yields those bytes for the uploader.
func TestParseSoraMultipart_FileUpload(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "kling-3-turbo")
	_ = mw.WriteField("seconds", "5")
	_ = mw.WriteField("size", "1280x720")
	fw, err := createFormFile(mw, "input_reference", "start.png", "image/png")
	if err != nil {
		t.Fatalf("multipart file: %v", err)
	}
	_, _ = fw.Write([]byte("raw png bytes"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/videos", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	sr, ct, body, err := parseSoraMultipart(req)
	if err != nil {
		t.Fatalf("parseSoraMultipart: %v", err)
	}
	if sr.Model != "kling-3-turbo" || sr.Seconds != 5 || sr.Width != 1280 {
		t.Errorf("fields: %+v", sr)
	}
	if ct != "image/png" {
		t.Errorf("content type: got %q want image/png", ct)
	}
	if string(body) != "raw png bytes" {
		t.Errorf("upload bytes: got %q", string(body))
	}
}

// createFormFile mirrors multipart.Writer.CreateFormFile but honors the
// explicit Content-Type header instead of forcing octet-stream.
func createFormFile(w *multipart.Writer, field, filename, contentType string) (io.Writer, error) {
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(
		`form-data; name=%q; filename=%q`, field, filename)}
	h["Content-Type"] = []string{contentType}
	return w.CreatePart(h)
}

// --- response envelope shape --------------------------------------------

// TestSoraViewFromJob_ExcludesForbiddenFields is the golden JSON test
// (§4.3): the response must contain the listed fields and MUST NOT
// contain the private ones (poll_url, cost, upstream_job_id,
// result_url, error_detail).
func TestSoraViewFromJob_ExcludesForbiddenFields(t *testing.T) {
	j := &domain.Job{
		ID:            "job_x",
		ModelAlias:    "kling-3-turbo",
		JST:           "text2video_kling",
		Status:        domain.JobCompleted,
		RequestTS:     time.Unix(1784570000, 0),
		FinishedAt:    time.Unix(1784570100, 0),
		UpstreamJobID: "up_secret",
		UpstreamCost:  12345,
		ResultURL:     "https://cdn.higgsfield.ai/x.mp4",
		RequestBodyJSON: `{"params":{"duration":5,"width":1280,"height":720}}`,
	}
	view := soraViewFromJob(j)
	blob, _ := json.Marshal(view)

	// Must-have.
	for _, want := range []string{`"id":"job_x"`, `"object":"video"`, `"model":"kling-3-turbo"`,
		`"status":"completed"`, `"progress":100`, `"created_at":1784570000`,
		`"completed_at":1784570100`, `"seconds":"5"`, `"size":"1280x720"`} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("response missing %s\nblob: %s", want, blob)
		}
	}
	// Must-not-have (leaks).
	for _, forbid := range []string{"poll_url", "cost", "upstream_job_id", "result_url", "error_detail"} {
		if strings.Contains(string(blob), forbid) {
			t.Errorf("response leaked forbidden field %q\nblob: %s", forbid, blob)
		}
	}
}

// TestSoraStatus maps every enum value to the correct Sora literal per §7.
func TestSoraStatus(t *testing.T) {
	cases := map[string]string{
		string(domain.JobPending):   "queued",
		string(domain.JobQueued):    "queued",
		string(domain.JobRunning):   "in_progress",
		string(domain.JobCompleted): "completed",
		string(domain.JobFailed):    "failed",
		string(domain.JobRefunded):  "failed",
		string(domain.JobTimeout):   "failed",
	}
	for in, want := range cases {
		if got := soraStatus(in); got != want {
			t.Errorf("soraStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- integration: create → upload wiring --------------------------------

// TestParseSoraRequest_DataURIRoutedThroughUploader posts a JSON body
// with a base64 data URI and asserts the uploader received the
// decoded bytes and its returned media_id landed on sr.MediaID.
// Exercises Handler.parseSoraRequest directly so the test does not
// need to stand up a full proxy.Service.
func TestParseSoraRequest_DataURIRoutedThroughUploader(t *testing.T) {
	png := []byte("\x89PNGtestbytes")
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)

	uploader := &fakeSoraUploader{mediaID: "media_from_test"}
	h := &Handler{SoraUploader: uploader}

	body := fmt.Sprintf(`{"model":"m","seconds":"5","size":"1280x720","input_reference":%q}`, dataURI)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	sr, err := h.parseSoraRequest(req)
	if err != nil {
		t.Fatalf("parseSoraRequest: %v", err)
	}
	if uploader.lastContentType != "image/png" {
		t.Errorf("uploader.lastContentType: got %q want image/png", uploader.lastContentType)
	}
	if !bytes.Equal(uploader.lastBytes, png) {
		t.Errorf("uploader.lastBytes mismatch")
	}
	if sr.MediaID != "media_from_test" {
		t.Errorf("MediaID: got %q want media_from_test", sr.MediaID)
	}
	if sr.ImageURL != "" {
		t.Errorf("ImageURL leaked after upload: %q", sr.ImageURL)
	}
}

// TestParseSoraRequest_UploadFailureSurfaces: the uploader returns an
// error → parseSoraRequest wraps it so the caller can render a 400.
func TestParseSoraRequest_UploadFailureSurfaces(t *testing.T) {
	png := []byte("bytes")
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	uploader := &fakeSoraUploader{err: errors.New("boom")}
	h := &Handler{SoraUploader: uploader}

	body := fmt.Sprintf(`{"model":"m","input_reference":%q}`, dataURI)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	_, err := h.parseSoraRequest(req)
	if err == nil {
		t.Fatalf("expected error from failed upload")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error not wrapped: %v", err)
	}
}

// TestParseSoraRequest_UploadWithoutUploader: a data-URI on a build
// without a SoraUploader wired must yield an explicit "requires
// SoraUploader" error rather than a nil-panic.
func TestParseSoraRequest_UploadWithoutUploader(t *testing.T) {
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("x"))
	h := &Handler{}
	body := fmt.Sprintf(`{"model":"m","input_reference":%q}`, dataURI)
	req := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	_, err := h.parseSoraRequest(req)
	if err == nil {
		t.Fatalf("expected error when no uploader wired")
	}
	if !strings.Contains(err.Error(), "SoraUploader") {
		t.Errorf("error missing uploader hint: %v", err)
	}
}

// --- poll (GET /v1/videos/{id}) -----------------------------------------

// TestSoraGet_ReturnsEnvelopeAndScopes verifies (a) a caller can fetch
// their own job in the Sora envelope, (b) another caller sees 404 —
// mirroring the /v1/jobs/{id} scoping rule.
func TestSoraGet_ReturnsEnvelopeAndScopes(t *testing.T) {
	store := &fakeSoraJobStore{byID: map[string]*domain.Job{
		"job_owned": {
			ID:         "job_owned",
			APIKeyID:   "key_owner",
			ModelAlias: "m",
			Status:     domain.JobCompleted,
			RequestTS:  time.Unix(1000, 0),
			FinishedAt: time.Unix(1100, 0),
			ResultURL:  "https://cdn/x.mp4",
		},
	}}
	h := &Handler{Jobs: store}

	// Owner sees the envelope.
	router := newSoraTestRouter(t, h, "key_owner")
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/job_owned", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET: status %d body=%q", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["status"] != "completed" || got["object"] != "video" {
		t.Errorf("envelope wrong: %+v", got)
	}
	if _, forbid := got["result_url"]; forbid {
		t.Errorf("result_url leaked into GET response: %+v", got)
	}

	// Non-owner sees 404.
	router2 := newSoraTestRouter(t, h, "key_intruder")
	req2 := httptest.NewRequest(http.MethodGet, "/v1/videos/job_owned", nil)
	rec2 := httptest.NewRecorder()
	router2.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("non-owner: got %d want 404", rec2.Code)
	}
}

// TestSoraGet_UnknownJobIs404 verifies the 404-not-found path.
func TestSoraGet_UnknownJobIs404(t *testing.T) {
	store := &fakeSoraJobStore{}
	h := &Handler{Jobs: store}
	router := newSoraTestRouter(t, h, "k")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", rec.Code)
	}
}

// --- content proxy (GET /v1/videos/{id}/content) ------------------------

// TestSoraContent_StreamsBodyAndForwardsRange spins up a fake upstream
// serving an MP4 with Range support and asserts (a) the caller
// receives the payload byte-for-byte, (b) Range headers pass through
// to the upstream, (c) the response headers include the CDN's own.
func TestSoraContent_StreamsBodyAndForwardsRange(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 1024)

	var gotRange string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if rng := r.Header.Get("Range"); rng != "" {
			// Fake range serve: return bytes 100-199.
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes 100-199/1024")
			w.Header().Set("Accept-Ranges", "bytes")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload[100:200])
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	store := &fakeSoraJobStore{byID: map[string]*domain.Job{
		"job_c": {
			ID:         "job_c",
			APIKeyID:   "k",
			Status:     domain.JobCompleted,
			ResultURL:  upstream.URL + "/x.mp4",
			ModelAlias: "m",
			RequestTS:  time.Unix(0, 0),
		},
	}}
	h := &Handler{Jobs: store}
	router := newSoraTestRouter(t, h, "k")

	// Full body.
	req := httptest.NewRequest(http.MethodGet, "/v1/videos/job_c/content", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("full-body: status %d body-len=%d", rec.Code, rec.Body.Len())
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("full-body: got %d bytes, want %d", rec.Body.Len(), len(payload))
	}
	if rec.Header().Get("Content-Type") != "video/mp4" {
		t.Errorf("Content-Type header not forwarded: %q", rec.Header().Get("Content-Type"))
	}

	// Range request.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/videos/job_c/content", nil)
	req2.Header.Set("Range", "bytes=100-199")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusPartialContent {
		t.Errorf("range: status %d want 206", rec2.Code)
	}
	if gotRange != "bytes=100-199" {
		t.Errorf("Range not forwarded upstream: got %q", gotRange)
	}
	if !bytes.Equal(rec2.Body.Bytes(), payload[100:200]) {
		t.Errorf("range payload mismatch")
	}
	if got := rec2.Header().Get("Content-Range"); got != "bytes 100-199/1024" {
		t.Errorf("Content-Range not forwarded: %q", got)
	}
}

// TestSoraContent_NotCompletedIs409 asserts callers polling for content
// on a queued/in-progress job see 409 rather than an empty body.
func TestSoraContent_NotCompletedIs409(t *testing.T) {
	store := &fakeSoraJobStore{byID: map[string]*domain.Job{
		"job_p": {ID: "job_p", APIKeyID: "k", Status: domain.JobQueued, ModelAlias: "m"},
	}}
	h := &Handler{Jobs: store}
	router := newSoraTestRouter(t, h, "k")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/job_p/content", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("status: got %d want 409", rec.Code)
	}
}

// TestSoraContent_MissingResultURLIs404 covers the edge where a job is
// completed but ResultURL somehow never landed (write race, upstream
// bug). The handler must 404 rather than 502 into a nil URL.
func TestSoraContent_MissingResultURLIs404(t *testing.T) {
	store := &fakeSoraJobStore{byID: map[string]*domain.Job{
		"job_e": {ID: "job_e", APIKeyID: "k", Status: domain.JobCompleted, ModelAlias: "m"},
	}}
	h := &Handler{Jobs: store}
	router := newSoraTestRouter(t, h, "k")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/job_e/content", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", rec.Code)
	}
}
