package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

// fakeRefresherRunner records TriggerOnce invocations. Uses atomic so the
// counter is safe if the handler ever fans out (it does not today, but
// this future-proofs the fake).
type fakeRefresherRunner struct {
	calls int32
}

func (f *fakeRefresherRunner) TriggerOnce(ctx context.Context) {
	atomic.AddInt32(&f.calls, 1)
}

// fakeRegressionRunner mirrors fakeRefresherRunner for the regression path.
type fakeRegressionRunner struct {
	calls int32
}

func (f *fakeRegressionRunner) TriggerOnce(ctx context.Context) {
	atomic.AddInt32(&f.calls, 1)
}

// newTickersRouter mounts the handler on a chi.Router for httptest.
func newTickersRouter(refresher RefresherRunner, regression RegressionRunner) chi.Router {
	r := chi.NewRouter()
	NewTickersHandler(refresher, regression, nil).Register(r)
	return r
}

func TestTickersHandler_Refresher(t *testing.T) {
	rf := &fakeRefresherRunner{}
	req := httptest.NewRequest(http.MethodPost, "/tickers/refresher", nil)
	w := httptest.NewRecorder()
	newTickersRouter(rf, nil).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := atomic.LoadInt32(&rf.calls); got != 1 {
		t.Errorf("TriggerOnce calls = %d, want 1", got)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["triggered"] != "refresher" {
		t.Errorf("triggered = %v, want refresher", body["triggered"])
	}
}

func TestTickersHandler_Regression(t *testing.T) {
	rg := &fakeRegressionRunner{}
	req := httptest.NewRequest(http.MethodPost, "/tickers/regression", nil)
	w := httptest.NewRecorder()
	newTickersRouter(nil, rg).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := atomic.LoadInt32(&rg.calls); got != 1 {
		t.Errorf("TriggerOnce calls = %d, want 1", got)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["triggered"] != "regression" {
		t.Errorf("triggered = %v, want regression", body["triggered"])
	}
}

func TestTickersHandler_NilRefresher(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/tickers/refresher", nil)
	w := httptest.NewRecorder()
	newTickersRouter(nil, nil).ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "unavailable" {
		t.Errorf("error.type = %v, want unavailable", errObj)
	}
}

func TestTickersHandler_NilRegression(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/tickers/regression", nil)
	w := httptest.NewRecorder()
	newTickersRouter(nil, nil).ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "unavailable" {
		t.Errorf("error.type = %v, want unavailable", errObj)
	}
}
