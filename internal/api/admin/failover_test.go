package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// -- fake stores ---------------------------------------------------------

type failoverFakeAccts struct {
	byID map[string]*domain.Account
	list []domain.Account
	// captured writes so tests can assert them.
	lastMarkID     string
	lastMarkStatus domain.AccountStatus
	lastMarkReason string
	resetCalls     []string
}

func (f *failoverFakeAccts) Get(_ context.Context, id string) (*domain.Account, error) {
	if a, ok := f.byID[id]; ok {
		copy := *a
		return &copy, nil
	}
	return nil, domain.ErrAccountNotFound
}
func (f *failoverFakeAccts) List(_ context.Context, filter ports.AccountFilter) ([]domain.Account, error) {
	if filter.Status == "" {
		return f.list, nil
	}
	out := []domain.Account{}
	for _, a := range f.list {
		if a.Status == filter.Status {
			out = append(out, a)
		}
	}
	return out, nil
}
func (f *failoverFakeAccts) Upsert(context.Context, *domain.Account) error { return nil }
func (f *failoverFakeAccts) UpdateBalance(context.Context, string, int64, int64, int64) error {
	return nil
}
func (f *failoverFakeAccts) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	return nil
}
func (f *failoverFakeAccts) UpdateInFlight(context.Context, string, int) error { return nil }
func (f *failoverFakeAccts) MarkStatus(_ context.Context, id string, s domain.AccountStatus, reason string) error {
	f.lastMarkID = id
	f.lastMarkStatus = s
	f.lastMarkReason = reason
	if a, ok := f.byID[id]; ok {
		a.Status = s
		a.StatusReason = reason
	}
	return nil
}
func (f *failoverFakeAccts) MarkThrottled(context.Context, string, time.Time, string) error {
	return nil
}
func (f *failoverFakeAccts) RecoverThrottled(context.Context) (int, error) { return 0, nil }
func (f *failoverFakeAccts) IncrFailStreak(context.Context, string) (int, error) {
	return 0, nil
}
func (f *failoverFakeAccts) ResetFailStreak(_ context.Context, id string) error {
	f.resetCalls = append(f.resetCalls, id)
	return nil
}
func (f *failoverFakeAccts) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	return nil, "", nil
}
func (f *failoverFakeAccts) Unlock(context.Context, string, string) error { return nil }

type failoverFakeEvents struct {
	inserts []struct {
		id, reason string
		kind       ports.FailoverEventKind
		status     int
	}
	counts      map[string]int // key = "id|kind"
	recentDisab int
	deleteCalls []string
}

func (f *failoverFakeEvents) Insert(_ context.Context, id string, kind ports.FailoverEventKind, reason string, status int) error {
	f.inserts = append(f.inserts, struct {
		id, reason string
		kind       ports.FailoverEventKind
		status     int
	}{id, reason, kind, status})
	return nil
}
func (f *failoverFakeEvents) Count(_ context.Context, id string, kind ports.FailoverEventKind, _ int) (int, error) {
	return f.counts[id+"|"+string(kind)], nil
}
func (f *failoverFakeEvents) CountRecentDisables(context.Context, int) (int, error) {
	return f.recentDisab, nil
}
func (f *failoverFakeEvents) List(context.Context, string, int) ([]ports.FailoverEventRow, error) {
	return nil, nil
}
func (f *failoverFakeEvents) DeleteForAccount(_ context.Context, id string) error {
	f.deleteCalls = append(f.deleteCalls, id)
	return nil
}

type failoverFakeOverrides struct {
	byID map[string]*ports.FailoverOverride
}

func (f *failoverFakeOverrides) Get(_ context.Context, id string) (*ports.FailoverOverride, error) {
	if o, ok := f.byID[id]; ok {
		return o, nil
	}
	return nil, nil
}
func (f *failoverFakeOverrides) Upsert(_ context.Context, o *ports.FailoverOverride) error {
	if f.byID == nil {
		f.byID = map[string]*ports.FailoverOverride{}
	}
	f.byID[o.AccountID] = o
	return nil
}
func (f *failoverFakeOverrides) Delete(_ context.Context, id string) error {
	delete(f.byID, id)
	return nil
}

// -- helper --------------------------------------------------------------

func newFailoverRouter(h *FailoverHandler) http.Handler {
	r := chi.NewRouter()
	h.Register(r)
	return r
}

// -- tests --------------------------------------------------------------

func TestFailover_GetConfig(t *testing.T) {
	cfg := &config.FailoverConfig{
		Enabled: true,
		Consecutive: config.ConsecutiveFailoverConfig{
			Enabled: true, FailLimit: 3,
		},
		Throttle: config.ThrottleFailoverConfig{
			RiskMarkers: []string{"blocked"},
		},
		OutageGuard: config.OutageGuardConfig{WindowSec: 30, DisableCountLimit: 3},
	}
	h := NewFailoverHandler(nil, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/failover/config", nil)
	newFailoverRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if !body["enabled"].(bool) {
		t.Errorf("enabled: got %v want true", body["enabled"])
	}
}

func TestFailover_PutConfig_PartialPatch(t *testing.T) {
	cfg := &config.FailoverConfig{
		Enabled: true,
		Consecutive: config.ConsecutiveFailoverConfig{
			Enabled: true, FailLimit: 3,
		},
	}
	h := NewFailoverHandler(nil, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"consecutive":{"fail_limit":7},"throttle":{"enabled":true,"risk_markers":["blocked","captcha"]}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/failover/config", bytes.NewBufferString(body))
	newFailoverRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: got %d want 200", rec.Code)
	}
	if cfg.Consecutive.FailLimit != 7 {
		t.Errorf("FailLimit after patch: got %d want 7", cfg.Consecutive.FailLimit)
	}
	if !cfg.Throttle.Enabled {
		t.Errorf("Throttle.Enabled after patch: got false")
	}
	if len(cfg.Throttle.RiskMarkers) != 2 || cfg.Throttle.RiskMarkers[0] != "blocked" {
		t.Errorf("RiskMarkers: got %v", cfg.Throttle.RiskMarkers)
	}
	// Untouched top-level field stays.
	if !cfg.Enabled {
		t.Errorf("Enabled clobbered by partial patch")
	}
}

func TestFailover_ListIsolated(t *testing.T) {
	accts := &failoverFakeAccts{
		list: []domain.Account{
			{ID: "acc-t", Email: "t@x", PlanType: domain.PlanPro,
				Status: domain.StatusThrottled, StatusReason: "throttle",
				ThrottledUntil: time.Now().Add(1 * time.Hour)},
			{ID: "acc-d", Email: "d@x", PlanType: domain.PlanPlus,
				Status: domain.StatusDisabled, StatusReason: "consec_fail"},
			{ID: "acc-a", Email: "a@x", PlanType: domain.PlanPro, Status: domain.StatusActive},
		},
	}
	cfg := &config.FailoverConfig{
		Enabled:  true,
		Throttle: config.ThrottleFailoverConfig{EvictWindowSec: 3600},
	}
	h := NewFailoverHandler(accts, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/failover/isolated", nil)
	newFailoverRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: got %d want 200", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	rows, _ := body["data"].([]any)
	if len(rows) != 2 {
		t.Errorf("rows: got %d want 2 (throttled + disabled only)", len(rows))
	}
}

func TestFailover_RecoverAccount(t *testing.T) {
	accts := &failoverFakeAccts{
		byID: map[string]*domain.Account{
			"acc-1": {ID: "acc-1", Status: domain.StatusDisabled},
		},
	}
	events := &failoverFakeEvents{}
	cfg := &config.FailoverConfig{Enabled: true}
	h := NewFailoverHandler(accts, events, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/acc-1/recover", nil)
	newFailoverRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if accts.lastMarkStatus != domain.StatusActive {
		t.Errorf("MarkStatus arg: got %s want active", accts.lastMarkStatus)
	}
	if accts.lastMarkReason == "" {
		t.Errorf("MarkStatus reason should be set (manual_recover), got empty")
	}
	if len(accts.resetCalls) != 1 || accts.resetCalls[0] != "acc-1" {
		t.Errorf("expected ResetFailStreak(acc-1), got %v", accts.resetCalls)
	}
	if len(events.deleteCalls) != 1 || events.deleteCalls[0] != "acc-1" {
		t.Errorf("expected DeleteForAccount(acc-1), got %v", events.deleteCalls)
	}
}

func TestFailover_RecoverAccount_RejectsActive(t *testing.T) {
	accts := &failoverFakeAccts{
		byID: map[string]*domain.Account{
			"acc-1": {ID: "acc-1", Status: domain.StatusActive},
		},
	}
	cfg := &config.FailoverConfig{Enabled: true}
	h := NewFailoverHandler(accts, nil, nil, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/acc-1/recover", nil)
	newFailoverRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("code: got %d want 409", rec.Code)
	}
}

func TestFailover_PutAccountFailover_Upserts(t *testing.T) {
	accts := &failoverFakeAccts{
		byID: map[string]*domain.Account{"acc-1": {ID: "acc-1", Status: domain.StatusActive}},
	}
	overrides := &failoverFakeOverrides{}
	cfg := &config.FailoverConfig{Enabled: true}
	h := NewFailoverHandler(accts, nil, overrides, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"fail_limit": 9, "enabled": false}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/accounts/acc-1/failover", bytes.NewBufferString(body))
	newFailoverRouter(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	got := overrides.byID["acc-1"]
	if got == nil {
		t.Fatal("override not persisted")
	}
	if got.FailLimit == nil || *got.FailLimit != 9 {
		t.Errorf("FailLimit: got %v", got.FailLimit)
	}
	if got.Enabled == nil || *got.Enabled != false {
		t.Errorf("Enabled: got %v want false", got.Enabled)
	}
}
