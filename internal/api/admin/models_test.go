package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeModelRegistry is a minimal ports.ModelRegistry for the ModelsHandler
// tests. It swaps its List result after Reload succeeds so tests can assert
// the previous_count / current_count delta reported by the handler.
type fakeModelRegistry struct {
	before      []*domain.ModelSpec
	after       []*domain.ModelSpec
	reloadErr   error
	reloadBlock time.Duration
	reloaded    int32
	// respectCtx, when true, causes Reload to honor ctx cancellation while
	// blocking for reloadBlock. This lets the timeout test observe that the
	// handler's 30s context cancels a longer artificial sleep.
	respectCtx bool
}

func (f *fakeModelRegistry) Resolve(alias string) (*domain.ModelSpec, error) {
	return nil, domain.ErrModelNotFound
}

func (f *fakeModelRegistry) List(_ ports.ModelFilter) []*domain.ModelSpec {
	if atomic.LoadInt32(&f.reloaded) > 0 {
		return f.after
	}
	return f.before
}

func (f *fakeModelRegistry) Reload(ctx context.Context) error {
	if f.reloadBlock > 0 {
		if f.respectCtx {
			select {
			case <-time.After(f.reloadBlock):
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			time.Sleep(f.reloadBlock)
		}
	}
	if f.reloadErr != nil {
		return f.reloadErr
	}
	atomic.AddInt32(&f.reloaded, 1)
	return nil
}

func (f *fakeModelRegistry) ResolveAlias(a string) (string, bool) { return "", false }
func (f *fakeModelRegistry) StarterLocked(_ string) bool          { return false }

func specSlice(n int) []*domain.ModelSpec {
	out := make([]*domain.ModelSpec, n)
	for i := 0; i < n; i++ {
		out[i] = &domain.ModelSpec{Alias: "m", Output: "image"}
	}
	return out
}

func newModelsRouter(reg ports.ModelRegistry) chi.Router {
	r := chi.NewRouter()
	NewModelsHandler(reg, nil).Register(r)
	return r
}

func TestModelsHandler_Reload_Success(t *testing.T) {
	reg := &fakeModelRegistry{
		before: specSlice(5),
		after:  specSlice(6),
	}

	req := httptest.NewRequest(http.MethodPost, "/models/reload", nil)
	w := httptest.NewRecorder()
	newModelsRouter(reg).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if got := body["previous_count"]; got != float64(5) {
		t.Errorf("previous_count = %v, want 5", got)
	}
	if got := body["current_count"]; got != float64(6) {
		t.Errorf("current_count = %v, want 6", got)
	}
	if _, ok := body["reloaded_at"].(string); !ok {
		t.Errorf("reloaded_at missing or not a string: %v", body["reloaded_at"])
	}
	if got := atomic.LoadInt32(&reg.reloaded); got != 1 {
		t.Errorf("Reload calls = %d, want 1", got)
	}
}

func TestModelsHandler_Reload_Failure(t *testing.T) {
	reg := &fakeModelRegistry{
		before:    specSlice(3),
		after:     specSlice(3),
		reloadErr: errors.New("parse: unexpected token"),
	}

	req := httptest.NewRequest(http.MethodPost, "/models/reload", nil)
	w := httptest.NewRecorder()
	newModelsRouter(reg).ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field missing: %v", body)
	}
	if errObj["type"] != "reload_failed" {
		t.Errorf("error.type = %v, want reload_failed", errObj["type"])
	}
	msg, _ := errObj["message"].(string)
	if msg == "" {
		t.Errorf("error.message is empty")
	}
}

// TestModelsHandler_Reload_ContextTimeout swaps the reload timeout for a
// short window and asserts the handler observes ctx cancellation from the
// fake's ctx-aware sleep. Reverts the constant on cleanup so package-level
// state stays consistent for the rest of the suite.
func TestModelsHandler_Reload_ContextTimeout(t *testing.T) {
	// The handler uses the package-level reloadTimeout. Swap it here for a
	// short window so the test does not have to wait 30 seconds. The
	// package-level state is restored on cleanup so subsequent tests see
	// the original value.
	original := reloadTimeoutVar
	reloadTimeoutVar = 50 * time.Millisecond
	t.Cleanup(func() { reloadTimeoutVar = original })

	reg := &fakeModelRegistry{
		before:      specSlice(1),
		after:       specSlice(1),
		reloadBlock: 2 * time.Second,
		respectCtx:  true,
	}

	req := httptest.NewRequest(http.MethodPost, "/models/reload", nil)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		newModelsRouter(reg).ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("handler did not return; ctx timeout should have unblocked Reload")
	}

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 on timeout; body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if msg == "" {
		t.Errorf("expected non-empty error.message on timeout")
	}
}
