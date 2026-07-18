package failover

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// mockAccountStore is a hand-rolled fake implementing ports.AccountStore
// with just enough surface for the failover controller. Every method
// the controller does NOT touch panics loudly so a change in the
// controller flow is caught immediately.
type mockAccountStore struct {
	mu sync.Mutex

	// state per account id.
	streaks   map[string]int
	statuses  map[string]domain.AccountStatus
	throttled map[string]time.Time
	reasons   map[string]string
}

func newMockAccountStore() *mockAccountStore {
	return &mockAccountStore{
		streaks:   map[string]int{},
		statuses:  map[string]domain.AccountStatus{},
		throttled: map[string]time.Time{},
		reasons:   map[string]string{},
	}
}

func (m *mockAccountStore) Get(_ context.Context, id string) (*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.statuses[id]; !ok {
		return nil, domain.ErrAccountNotFound
	}
	return &domain.Account{
		ID:             id,
		Status:         m.statuses[id],
		FailStreak:     m.streaks[id],
		ThrottledUntil: m.throttled[id],
		StatusReason:   m.reasons[id],
	}, nil
}
func (m *mockAccountStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	return nil, nil
}
func (m *mockAccountStore) Upsert(context.Context, *domain.Account) error { return nil }
func (m *mockAccountStore) UpdateBalance(context.Context, string, int64, int64, int64) error {
	return nil
}
func (m *mockAccountStore) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	return nil
}
func (m *mockAccountStore) UpdateInFlight(context.Context, string, int) error { return nil }

func (m *mockAccountStore) MarkStatus(_ context.Context, id string, s domain.AccountStatus, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[id] = s
	m.reasons[id] = reason
	if s == domain.StatusActive {
		delete(m.throttled, id)
	}
	return nil
}
func (m *mockAccountStore) MarkThrottled(_ context.Context, id string, until time.Time, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[id] = domain.StatusThrottled
	m.throttled[id] = until
	m.reasons[id] = reason
	return nil
}
func (m *mockAccountStore) RecoverThrottled(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	n := 0
	for id, deadline := range m.throttled {
		if !deadline.IsZero() && !deadline.After(now) && m.statuses[id] == domain.StatusThrottled {
			m.statuses[id] = domain.StatusActive
			delete(m.throttled, id)
			m.reasons[id] = ""
			n++
		}
	}
	return n, nil
}
func (m *mockAccountStore) IncrFailStreak(_ context.Context, id string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.statuses[id]; !ok {
		return 0, domain.ErrAccountNotFound
	}
	m.streaks[id]++
	return m.streaks[id], nil
}
func (m *mockAccountStore) ResetFailStreak(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.statuses[id]; !ok {
		return domain.ErrAccountNotFound
	}
	m.streaks[id] = 0
	return nil
}
func (m *mockAccountStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	return nil, "", nil
}
func (m *mockAccountStore) Unlock(context.Context, string, string) error { return nil }

// seed inserts an active account with zero streak so IncrFailStreak has
// a row to update.
func (m *mockAccountStore) seed(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses[id] = domain.StatusActive
	m.streaks[id] = 0
}

// getStreak / getStatus / getReason accessors for assertions.
func (m *mockAccountStore) getStreak(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streaks[id]
}
func (m *mockAccountStore) getStatus(id string) domain.AccountStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statuses[id]
}
func (m *mockAccountStore) getReason(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reasons[id]
}
func (m *mockAccountStore) getThrottled(id string) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.throttled[id]
}

// mockEventStore is a hand-rolled in-memory ports.FailoverEventStore
// with a clock injection so tests can precisely position events inside
// the sliding windows.
type mockEventStore struct {
	mu     sync.Mutex
	rows   []mockEvent
	nextID int64
	now    func() time.Time
}

type mockEvent struct {
	id         int64
	accountID  string
	kind       ports.FailoverEventKind
	reason     string
	httpStatus int
	createdAt  time.Time
}

func newMockEventStore() *mockEventStore {
	return &mockEventStore{now: time.Now}
}

func (m *mockEventStore) Insert(_ context.Context, accountID string, kind ports.FailoverEventKind, reason string, httpStatus int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	m.rows = append(m.rows, mockEvent{
		id:         m.nextID,
		accountID:  accountID,
		kind:       kind,
		reason:     reason,
		httpStatus: httpStatus,
		createdAt:  m.now(),
	})
	return nil
}

func (m *mockEventStore) Count(_ context.Context, accountID string, kind ports.FailoverEventKind, windowSec int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if windowSec <= 0 {
		return 0, nil
	}
	cutoff := m.now().Add(-time.Duration(windowSec) * time.Second)
	n := 0
	for _, r := range m.rows {
		if r.accountID == accountID && r.kind == kind && !r.createdAt.Before(cutoff) {
			n++
		}
	}
	return n, nil
}

func (m *mockEventStore) CountRecentDisables(_ context.Context, windowSec int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if windowSec <= 0 {
		return 0, nil
	}
	cutoff := m.now().Add(-time.Duration(windowSec) * time.Second)
	seen := map[string]struct{}{}
	for _, r := range m.rows {
		if (r.reason == ReasonConsecFail || r.reason == ReasonEvict) && !r.createdAt.Before(cutoff) {
			seen[r.accountID] = struct{}{}
		}
	}
	return len(seen), nil
}

func (m *mockEventStore) List(_ context.Context, accountID string, limit int) ([]ports.FailoverEventRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ports.FailoverEventRow{}
	for i := len(m.rows) - 1; i >= 0 && len(out) < limit; i-- {
		r := m.rows[i]
		if r.accountID != accountID {
			continue
		}
		out = append(out, ports.FailoverEventRow{
			ID:         r.id,
			AccountID:  r.accountID,
			Kind:       r.kind,
			Reason:     r.reason,
			HTTPStatus: r.httpStatus,
			CreatedAt:  r.createdAt,
		})
	}
	return out, nil
}

func (m *mockEventStore) DeleteForAccount(_ context.Context, accountID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.rows[:0]
	for _, r := range m.rows {
		if r.accountID != accountID {
			kept = append(kept, r)
		}
	}
	m.rows = kept
	return nil
}

// countByKind is a test helper that returns how many events of a given
// kind exist for account id, ignoring the window entirely.
func (m *mockEventStore) countByKind(id string, kind ports.FailoverEventKind) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.accountID == id && r.kind == kind {
			n++
		}
	}
	return n
}

// stubNotifier records every Send call so tests can assert we alerted.
type stubNotifier struct {
	mu    sync.Mutex
	calls []ports.Notification
}

func (n *stubNotifier) Send(_ context.Context, msg ports.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, msg)
	return nil
}
func (n *stubNotifier) Name() string { return "stub" }
func (n *stubNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

// newController builds a Controller with sensible defaults for tests.
func newController(t *testing.T, cfg *config.FailoverConfig, accts *mockAccountStore, evs *mockEventStore) *Controller {
	t.Helper()
	if cfg == nil {
		cfg = &config.FailoverConfig{
			Enabled: true,
			Consecutive: config.ConsecutiveFailoverConfig{
				Enabled:   true,
				FailLimit: 3,
			},
			Throttle: config.ThrottleFailoverConfig{
				Enabled:        true,
				JudgeWindowSec: 60,
				JudgeCount:     3,
				CooldownSec:    60,
				EvictWindowSec: 3600,
				EvictCount:     2,
			},
			OutageGuard: config.OutageGuardConfig{
				WindowSec:         30,
				DisableCountLimit: 3,
			},
		}
	}
	// Silent logger — the controller's log lines don't matter to test
	// assertions, only its state mutations.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Controller{
		Accounts: accts,
		Events:   evs,
		Cfg:      cfg,
		Logger:   logger,
	}
}

// -- tests ----------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Classification
	}{
		{"nil", nil, ClassIgnore},
		{"401", domain.ErrUpstreamUnauthorized, ClassAccountAttributable},
		{"429", domain.ErrUpstreamRateLimit, ClassThrottle},
		{"403", domain.ErrUpstreamForbidden, ClassIgnore},
		{"422", domain.ErrUpstreamBadBody, ClassIgnore},
		{"5xx", domain.ErrUpstreamServerError, ClassIgnore},
		{"timeout", domain.ErrUpstreamTimeout, ClassIgnore},
		{"context deadline", context.DeadlineExceeded, ClassIgnore},
		{"context canceled", context.Canceled, ClassIgnore},
		{"conn reset", syscall.ECONNRESET, ClassAccountAttributable},
		{"epipe", syscall.EPIPE, ClassAccountAttributable},
		{"datadome body", errors.New("datadome challenge required"), ClassAccountAttributable},
		{"captcha body", errors.New("please solve captcha"), ClassAccountAttributable},
		{"unknown string", errors.New("weird thing"), ClassIgnore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			if got != tc.want {
				t.Errorf("Classify(%q) = %v want %v", tc.name, got, tc.want)
			}
		})
	}
	// net.Error path.
	nErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("boom")}
	if got := Classify(nErr); got != ClassAccountAttributable {
		t.Errorf("Classify(net.OpError) = %v want ClassAccountAttributable", got)
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Classification
	}{
		{401, "", ClassAccountAttributable},
		{429, "", ClassThrottle},
		{403, "", ClassIgnore},
		{422, "", ClassIgnore},
		{500, "", ClassIgnore},
		{502, "", ClassIgnore},
		{200, "please solve captcha", ClassAccountAttributable},
		{200, "datadome challenge", ClassAccountAttributable},
		{418, "", ClassIgnore},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status)+":"+tc.body, func(t *testing.T) {
			if got := ClassifyStatus(tc.status, tc.body); got != tc.want {
				t.Errorf("ClassifyStatus(%d,%q) = %v want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestRecordSuccess_ClearsStreak(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	ctl := newController(t, nil, accts, evs)

	// bump the streak the raw way, verify RecordSuccess clears it.
	if _, err := accts.IncrFailStreak(context.Background(), "acc-1"); err != nil {
		t.Fatal(err)
	}
	if accts.getStreak("acc-1") != 1 {
		t.Fatalf("streak setup: got %d want 1", accts.getStreak("acc-1"))
	}
	ctl.RecordSuccess(context.Background(), "acc-1")
	if got := accts.getStreak("acc-1"); got != 0 {
		t.Errorf("streak after RecordSuccess: got %d want 0", got)
	}
}

func TestRecordFailure_DisablesAtLimit(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	ctl := newController(t, nil, accts, evs)

	ctx := context.Background()
	// FailLimit is 3 in the default cfg above.
	ctl.RecordFailure(ctx, "acc-1", 401, "auth failure")
	ctl.RecordFailure(ctx, "acc-1", 401, "auth failure")
	if got := accts.getStatus("acc-1"); got != domain.StatusActive {
		t.Fatalf("after 2 failures status: got %s want active", got)
	}
	ctl.RecordFailure(ctx, "acc-1", 401, "auth failure")
	if got := accts.getStatus("acc-1"); got != domain.StatusDisabled {
		t.Fatalf("after 3 failures status: got %s want disabled", got)
	}
	if got := accts.getReason("acc-1"); got != ReasonConsecFail {
		t.Errorf("reason: got %q want %q", got, ReasonConsecFail)
	}
	// 4 failure-kind events written (3 IncrFailStreak paths + 1 disable
	// marker written from within disable() before flipping status).
	if got := evs.countByKind("acc-1", ports.FailoverEventFailure); got != 4 {
		t.Errorf("failure events written: got %d want 4", got)
	}
}

func TestRecordFailure_IgnoredWhenDisabled(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	cfg := &config.FailoverConfig{
		Enabled:     false,
		Consecutive: config.ConsecutiveFailoverConfig{Enabled: true, FailLimit: 1},
		OutageGuard: config.OutageGuardConfig{WindowSec: 30, DisableCountLimit: 3},
	}
	ctl := newController(t, cfg, accts, evs)
	ctl.RecordFailure(context.Background(), "acc-1", 401, "")
	if accts.getStatus("acc-1") == domain.StatusDisabled {
		t.Fatal("disable happened while cfg.Enabled=false")
	}
	if evs.countByKind("acc-1", ports.FailoverEventFailure) != 0 {
		t.Errorf("events written while disabled: got %d", evs.countByKind("acc-1", ports.FailoverEventFailure))
	}
}

func TestRecordThrottle_MarksThrottledOnWindowHit(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	ctl := newController(t, nil, accts, evs)

	// Freeze clock so the cooldown deadline is deterministic.
	fixed := time.Unix(1_700_000_000, 0)
	ctl.Clock = func() time.Time { return fixed }
	evs.now = func() time.Time { return fixed }

	ctx := context.Background()
	// Default JudgeCount = 3.
	ctl.RecordThrottle(ctx, "acc-1", "")
	ctl.RecordThrottle(ctx, "acc-1", "")
	if got := accts.getStatus("acc-1"); got != domain.StatusActive {
		t.Fatalf("after 2 throttles status: got %s want active", got)
	}
	ctl.RecordThrottle(ctx, "acc-1", "")
	if got := accts.getStatus("acc-1"); got != domain.StatusThrottled {
		t.Fatalf("after 3 throttles status: got %s want throttled", got)
	}
	deadline := accts.getThrottled("acc-1")
	wantDeadline := fixed.Add(60 * time.Second)
	if !deadline.Equal(wantDeadline) {
		t.Errorf("throttled_until: got %v want %v", deadline, wantDeadline)
	}
}

func TestRecordThrottle_EvictsAfterRepeatedBlacklists(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	cfg := &config.FailoverConfig{
		Enabled: true,
		Consecutive: config.ConsecutiveFailoverConfig{
			Enabled: true, FailLimit: 3,
		},
		Throttle: config.ThrottleFailoverConfig{
			Enabled:        true,
			JudgeWindowSec: 60,
			JudgeCount:     1, // every throttle immediately blacklists
			CooldownSec:    30,
			EvictWindowSec: 3600,
			EvictCount:     2,
		},
		OutageGuard: config.OutageGuardConfig{WindowSec: 30, DisableCountLimit: 10},
	}
	ctl := newController(t, cfg, accts, evs)
	// First throttle -> immediate cooldown + one blacklist event.
	ctl.RecordThrottle(context.Background(), "acc-1", "")
	if got := accts.getStatus("acc-1"); got != domain.StatusThrottled {
		t.Fatalf("after 1 throttle: got status %s want throttled", got)
	}
	// Second throttle -> another blacklist -> hits EvictCount=2 -> disable.
	ctl.RecordThrottle(context.Background(), "acc-1", "")
	if got := accts.getStatus("acc-1"); got != domain.StatusDisabled {
		t.Fatalf("after 2 throttles: got status %s want disabled", got)
	}
	if got := accts.getReason("acc-1"); got != ReasonEvict {
		t.Errorf("reason: got %q want %q", got, ReasonEvict)
	}
}

func TestOutageGuardSuppressesDisable(t *testing.T) {
	accts := newMockAccountStore()
	// Three "already disabled" accounts land as recent consec_fail events
	// so the guard trips immediately when acc-1 tries to disable.
	accts.seed("acc-1")
	accts.seed("acc-2")
	accts.seed("acc-3")
	accts.seed("acc-4")
	accts.statuses["acc-2"] = domain.StatusDisabled
	accts.statuses["acc-3"] = domain.StatusDisabled
	accts.statuses["acc-4"] = domain.StatusDisabled

	evs := newMockEventStore()
	notifier := &stubNotifier{}
	cfg := &config.FailoverConfig{
		Enabled: true,
		Consecutive: config.ConsecutiveFailoverConfig{
			Enabled: true, FailLimit: 1,
		},
		OutageGuard: config.OutageGuardConfig{
			WindowSec:         60,
			DisableCountLimit: 3,
		},
	}
	// Seed three recent consec_fail events under three distinct accts.
	for _, id := range []string{"acc-2", "acc-3", "acc-4"} {
		_ = evs.Insert(context.Background(), id, ports.FailoverEventFailure, ReasonConsecFail, 0)
	}
	ctl := newController(t, cfg, accts, evs)
	ctl.Notifier = notifier

	ctl.RecordFailure(context.Background(), "acc-1", 401, "")
	if got := accts.getStatus("acc-1"); got == domain.StatusDisabled {
		t.Fatalf("outage guard should have suppressed disable, got status %s", got)
	}
	// A failure event was still written (paper trail requirement).
	if evs.countByKind("acc-1", ports.FailoverEventFailure) != 1 {
		t.Errorf("expected 1 failure event, got %d", evs.countByKind("acc-1", ports.FailoverEventFailure))
	}
	if notifier.count() == 0 {
		t.Errorf("expected a warn notification when the guard suppresses a disable")
	}
}

func TestRecordError_IgnoresBenign(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	ctl := newController(t, nil, accts, evs)
	ctx := context.Background()
	for _, e := range []error{
		domain.ErrUpstreamForbidden,
		domain.ErrUpstreamBadBody,
		domain.ErrUpstreamServerError,
		domain.ErrUpstreamTimeout,
		context.DeadlineExceeded,
	} {
		ctl.RecordError(ctx, "acc-1", e, "")
	}
	if got := accts.getStreak("acc-1"); got != 0 {
		t.Errorf("benign errors bumped streak: got %d want 0", got)
	}
	if got := accts.getStatus("acc-1"); got != domain.StatusActive {
		t.Errorf("benign errors changed status: got %s want active", got)
	}
	if len(evs.rows) != 0 {
		t.Errorf("benign errors wrote events: got %d rows", len(evs.rows))
	}
}

func TestRecordError_401RoutesToConsecutive(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	cfg := &config.FailoverConfig{
		Enabled:     true,
		Consecutive: config.ConsecutiveFailoverConfig{Enabled: true, FailLimit: 2},
		OutageGuard: config.OutageGuardConfig{WindowSec: 60, DisableCountLimit: 10},
	}
	ctl := newController(t, cfg, accts, evs)
	ctl.RecordError(context.Background(), "acc-1", domain.ErrUpstreamUnauthorized, "")
	ctl.RecordError(context.Background(), "acc-1", domain.ErrUpstreamUnauthorized, "")
	if got := accts.getStatus("acc-1"); got != domain.StatusDisabled {
		t.Errorf("expected disabled after 2 401s, got %s", got)
	}
}

func TestNilController_NoPanic(t *testing.T) {
	var ctl *Controller
	// Every entry point should tolerate a nil receiver.
	ctl.RecordSuccess(context.Background(), "acc-1")
	ctl.RecordFailure(context.Background(), "acc-1", 401, "boom")
	ctl.RecordThrottle(context.Background(), "acc-1", "body")
	ctl.RecordError(context.Background(), "acc-1", domain.ErrUpstreamUnauthorized, "")
}

func TestPerAccountOverride_TakesPrecedence(t *testing.T) {
	accts := newMockAccountStore()
	accts.seed("acc-1")
	evs := newMockEventStore()
	cfg := &config.FailoverConfig{
		Enabled: true,
		Consecutive: config.ConsecutiveFailoverConfig{
			Enabled: true, FailLimit: 3,
		},
		OutageGuard: config.OutageGuardConfig{WindowSec: 60, DisableCountLimit: 10},
	}
	ctl := newController(t, cfg, accts, evs)
	// Attach a stub override store that lowers the limit to 1.
	one := 1
	trueVal := true
	ctl.Overrides = &stubOverridesStore{
		byID: map[string]*ports.FailoverOverride{
			"acc-1": {AccountID: "acc-1", Enabled: &trueVal, FailLimit: &one},
		},
	}
	ctl.RecordFailure(context.Background(), "acc-1", 401, "")
	if got := accts.getStatus("acc-1"); got != domain.StatusDisabled {
		t.Errorf("per-account fail_limit=1 should have disabled on first failure, got %s", got)
	}
}

// stubOverridesStore is a minimal ports.FailoverOverridesStore.
type stubOverridesStore struct {
	byID map[string]*ports.FailoverOverride
}

func (s *stubOverridesStore) Get(_ context.Context, id string) (*ports.FailoverOverride, error) {
	if o, ok := s.byID[id]; ok {
		return o, nil
	}
	return nil, nil
}
func (s *stubOverridesStore) Upsert(_ context.Context, o *ports.FailoverOverride) error {
	if s.byID == nil {
		s.byID = map[string]*ports.FailoverOverride{}
	}
	s.byID[o.AccountID] = o
	return nil
}
func (s *stubOverridesStore) Delete(_ context.Context, id string) error {
	delete(s.byID, id)
	return nil
}
