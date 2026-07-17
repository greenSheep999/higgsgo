package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

func newTestDispatcher(t *testing.T, key string) *Dispatcher {
	t.Helper()
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(lg, Config{
		SigningKey:     key,
		Timeout:        2 * time.Second,
		MaxRetry:       2,
		InitialBackoff: 20 * time.Millisecond,
		Concurrency:    4,
	})
}

func TestFireDeliversSignedPayload(t *testing.T) {
	var (
		mu       sync.Mutex
		receipts []map[string]any
		sigs     []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		sigs = append(sigs, r.Header.Get("X-Higgsgo-Signature"))
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		receipts = append(receipts, m)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, "s3cret")
	job := &domain.Job{
		ID: "job-1", ModelAlias: "seedance-2-0-mini", APIKeyID: "key-x",
		Status: domain.JobCompleted, ResultURL: "https://cdn/x.mp4",
		UpstreamCost: 1500, LatencyMS: 12345,
		FinishedAt: time.Now().UTC(), RequestTS: time.Now().UTC().Add(-30 * time.Second),
	}
	d.Fire(srv.URL, job)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(receipts) != 1 {
		t.Fatalf("expected 1 receipt, got %d", len(receipts))
	}
	if receipts[0]["event"] != "job.completed" {
		t.Errorf("event mismatch: %v", receipts[0]["event"])
	}
	if receipts[0]["job_id"] != "job-1" {
		t.Errorf("job_id mismatch: %v", receipts[0]["job_id"])
	}
	if sigs[0] == "" || sigs[0][:7] != "sha256=" {
		t.Errorf("signature missing/mis-shaped: %q", sigs[0])
	}
}

func TestFireRetriesOnServerError(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	d := newTestDispatcher(t, "")
	d.Fire(srv.URL, &domain.Job{ID: "job-2", ModelAlias: "x", Status: domain.JobFailed})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", attempts)
	}
}
