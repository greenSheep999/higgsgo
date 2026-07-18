package admin

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/go-chi/chi/v5"
)

// triggerTimeout caps a single admin-triggered tick so a misbehaving
// upstream cannot pin a request indefinitely.
const triggerTimeout = 30 * time.Second

// RefresherRunner is the narrow slice of *refresher.Refresher used by the
// tickers handler. Declared here so admin does not import the core
// refresher package and the handler can be exercised with a fake in tests.
type RefresherRunner interface {
	TriggerOnce(ctx context.Context)
}

// RegressionRunner is the narrow slice of *regression.Ticker used by the
// tickers handler. Same rationale as RefresherRunner.
type RegressionRunner interface {
	TriggerOnce(ctx context.Context)
}

// TickersHandler serves /admin/tickers/* manual-trigger endpoints. Both
// runners are optional: if a runner is nil the corresponding endpoint
// answers 503 unavailable so operators get a clear signal instead of a
// silent 404.
type TickersHandler struct {
	Refresher  RefresherRunner
	Regression RegressionRunner
	Logger     *slog.Logger
}

// NewTickersHandler builds a handler over the given runners. Either may
// be nil. main.go passes a typed nil pointer (`var tk *regression.Ticker`)
// when the ticker is disabled — that lands here as a non-nil interface
// wrapping a nil concrete pointer, so a plain `== nil` check in the
// handler silently passes and TriggerOnce panics with a nil receiver.
// Normalise both runners here so the handler's nil check works.
func NewTickersHandler(refresher RefresherRunner, regression RegressionRunner, logger *slog.Logger) *TickersHandler {
	if isNilInterface(refresher) {
		refresher = nil
	}
	if isNilInterface(regression) {
		regression = nil
	}
	return &TickersHandler{
		Refresher:  refresher,
		Regression: regression,
		Logger:     logger,
	}
}

// isNilInterface returns true both for a literal-nil interface and for
// an interface value wrapping a nil pointer / channel / map / slice.
func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Chan, reflect.Func, reflect.Interface,
		reflect.Map, reflect.Slice:
		return rv.IsNil()
	}
	return false
}

// Register mounts the routes under /admin/tickers.
func (h *TickersHandler) Register(r chi.Router) {
	r.Post("/tickers/refresher", h.TriggerRefresher)
	r.Post("/tickers/regression", h.TriggerRegression)
}

// TriggerRefresher runs a single refresher tick synchronously. Returns
// 200 on success, 503 when the refresher is not wired.
func (h *TickersHandler) TriggerRefresher(w http.ResponseWriter, r *http.Request) {
	if h.Refresher == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "refresher not configured")
		return
	}
	if h.Logger != nil {
		h.Logger.Info("admin trigger refresher")
	}
	ctx, cancel := context.WithTimeout(r.Context(), triggerTimeout)
	defer cancel()
	h.Refresher.TriggerOnce(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"triggered": "refresher",
	})
}

// TriggerRegression runs a single regression tick synchronously. Returns
// 200 on success, 503 when the regression ticker is not wired.
func (h *TickersHandler) TriggerRegression(w http.ResponseWriter, r *http.Request) {
	if h.Regression == nil {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "regression ticker not configured")
		return
	}
	if h.Logger != nil {
		h.Logger.Info("admin trigger regression")
	}
	ctx, cancel := context.WithTimeout(r.Context(), triggerTimeout)
	defer cancel()
	h.Regression.TriggerOnce(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"triggered": "regression",
	})
}
