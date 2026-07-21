package upstream

// Tests for UploadImage — the three-step protocol documented in
// docs/OPENAI-VIDEO-COMPAT.md Appendix C.
//
// UploadImage is used by the /v1/videos Sora surface to convert
// data-URI / multipart file uploads into a higgsfield media_id.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestUploadImage_HappyPath drives the full reserve → PUT → commit
// sequence and asserts the returned media_id is what /media handed us
// back. Uses two httptest servers so the S3 upload URL points at a
// distinct host from the higgsfield API.
func TestUploadImage_HappyPath(t *testing.T) {
	var (
		reserveHits atomic.Int64
		putHits     atomic.Int64
		commitHits  atomic.Int64
	)
	// Fake S3 upload target. Serves 200 to any PUT.
	//
	// Also asserts no Authorization header leaks out: the upload URL
	// is a presigned S3 PUT and higgsfield's bearer must NOT reach S3
	// (Appendix C, "no higgsfield auth header on this request").
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("s3: unexpected method %s", r.Method)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("s3: Authorization leaked: %q", auth)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "hello-image-bytes" {
			t.Errorf("s3: body got %q want hello-image-bytes", string(body))
		}
		if got := r.Header.Get("Content-Type"); got != "image/jpeg" {
			t.Errorf("s3: content-type got %q want image/jpeg", got)
		}
		putHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer s3.Close()

	// Fake higgsfield API. Handles /media (reserve) + /media/{id}/upload (commit).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/media":
			reserveHits.Add(1)
			if got := r.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("reserve: content-type got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"media_abc","url":"https://cdn/higgs/final","upload_url":"` +
				s3.URL + `/put"}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/media/") && strings.HasSuffix(r.URL.Path, "/upload"):
			commitHits.Add(1)
			body, _ := io.ReadAll(r.Body)
			if strings.TrimSpace(string(body)) != "{}" {
				t.Errorf("commit body: got %q want {}", string(body))
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("upstream: unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	c, _ := newRetryTestClient(t, upstream)
	mediaID, err := c.UploadImage(context.Background(), testAccount(), "image/jpeg",
		bytes.NewReader([]byte("hello-image-bytes")))
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if mediaID != "media_abc" {
		t.Errorf("mediaID: got %q want media_abc", mediaID)
	}
	if reserveHits.Load() != 1 {
		t.Errorf("reserve hits: got %d want 1", reserveHits.Load())
	}
	if putHits.Load() != 1 {
		t.Errorf("put hits: got %d want 1", putHits.Load())
	}
	if commitHits.Load() != 1 {
		t.Errorf("commit hits: got %d want 1", commitHits.Load())
	}
}

// TestUploadImage_ReserveFailure surfaces a "media_reserve_failed"
// wrapper when the /media POST returns non-2xx.
func TestUploadImage_ReserveFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "policy denied", http.StatusForbidden)
	}))
	defer upstream.Close()
	c, _ := newRetryTestClient(t, upstream)
	_, err := c.UploadImage(context.Background(), testAccount(), "image/jpeg",
		bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatalf("expected error on reserve failure")
	}
	if !strings.Contains(err.Error(), "media_reserve_failed") {
		t.Errorf("error not tagged: %v", err)
	}
}

// TestUploadImage_UploadFailure surfaces "media_upload_failed" when
// the S3 PUT returns non-2xx.
func TestUploadImage_UploadFailure(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "s3 denied", http.StatusForbidden)
	}))
	defer s3.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media" && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m1","url":"https://cdn/f","upload_url":"` + s3.URL + `/x"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	c, _ := newRetryTestClient(t, upstream)
	_, err := c.UploadImage(context.Background(), testAccount(), "image/png",
		bytes.NewReader([]byte("bytes")))
	if err == nil {
		t.Fatalf("expected error on S3 failure")
	}
	if !strings.Contains(err.Error(), "media_upload_failed") {
		t.Errorf("error not tagged: %v", err)
	}
}

// TestUploadImage_CommitFailure surfaces "media_commit_failed" when
// the commit POST returns non-2xx.
func TestUploadImage_CommitFailure(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer s3.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/media" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m1","url":"https://cdn/f","upload_url":"` + s3.URL + `/x"}`))
		case strings.HasSuffix(r.URL.Path, "/upload"):
			http.Error(w, "commit failed", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	c, _ := newRetryTestClient(t, upstream)
	_, err := c.UploadImage(context.Background(), testAccount(), "image/png",
		bytes.NewReader([]byte("bytes")))
	if err == nil {
		t.Fatalf("expected error on commit failure")
	}
	if !strings.Contains(err.Error(), "media_commit_failed") {
		t.Errorf("error not tagged: %v", err)
	}
}
