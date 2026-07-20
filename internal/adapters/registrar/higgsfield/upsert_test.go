//go:build register
// +build register

// Verifies storeAdapter.MarkCompleted's ROADMAP §5.4 P4-3c behaviour:
// a successful driver.Register call flows a fully-populated Account
// row into the AccountStore before flipping the registrations row to
// success. Runs under -tags register because storeAdapter only
// compiles in the register-enabled build (that's where the plugin
// import lives).
//
// Uses fake in-memory stores so the test is fast and hermetic. Real
// SQLite integration is covered by registration_store_test.go +
// account_store tests in internal/adapters/storage/sqlite/.

package higgsfield

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	register "github.com/greensheep999/higgsgo/plugins/register"
)

// fakeRegStore is the minimal ports.RegistrationStore stub needed
// for the MarkCompleted path — panics on anything not exercised so
// a future adapter change fails loud.
type fakeRegStore struct {
	rows map[int64]*ports.Registration
	// captures the last MarkCompleted args
	completedID int64
	completedAcc string
}

func newFakeRegStore() *fakeRegStore {
	return &fakeRegStore{rows: map[int64]*ports.Registration{}}
}

func (s *fakeRegStore) Enqueue(_ context.Context, _ *ports.Registration) error {
	panic("unexpected Enqueue")
}
func (s *fakeRegStore) NextPending(_ context.Context) (*ports.Registration, error) {
	panic("unexpected NextPending")
}
func (s *fakeRegStore) MarkRunning(_ context.Context, _ int64) error { panic("unexpected MarkRunning") }
func (s *fakeRegStore) MarkCompleted(_ context.Context, id int64, accountID string) error {
	s.completedID = id
	s.completedAcc = accountID
	if r, ok := s.rows[id]; ok {
		r.Status = "success"
		r.AccountID = accountID
	}
	return nil
}
func (s *fakeRegStore) MarkFailed(_ context.Context, _ int64, _ string) error {
	panic("unexpected MarkFailed")
}
func (s *fakeRegStore) Get(_ context.Context, id int64) (*ports.Registration, error) {
	r, ok := s.rows[id]
	if !ok {
		return nil, domain.ErrRegistrationNotFound
	}
	return r, nil
}
func (s *fakeRegStore) List(_ context.Context, _ ports.RegistrationFilter) ([]ports.Registration, error) {
	panic("unexpected List")
}
func (s *fakeRegStore) ResetToPending(_ context.Context, _ int64) error {
	panic("unexpected ResetToPending")
}
func (s *fakeRegStore) ReclaimStaleRunning(_ context.Context) (int64, error) {
	panic("unexpected ReclaimStaleRunning")
}

// fakeAccountStore records the Upsert call so the test can inspect
// the produced Account. Every other method panics.
type fakeAccountStore struct {
	upserted  *domain.Account
	upsertErr error
}

func (f *fakeAccountStore) Get(context.Context, string) (*domain.Account, error) {
	return nil, domain.ErrAccountNotFound
}
func (f *fakeAccountStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	panic("unexpected List")
}
func (f *fakeAccountStore) Upsert(_ context.Context, a *domain.Account) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	// Copy so downstream mutation cannot poison the recorded call.
	cp := *a
	f.upserted = &cp
	return nil
}
func (f *fakeAccountStore) UpdateBalance(context.Context, string, int64, int64, int64) error {
	panic("unexpected UpdateBalance")
}
func (f *fakeAccountStore) UpdateEntitlements(context.Context, string, ports.EntitlementUpdate) error {
	panic("unexpected UpdateEntitlements")
}
func (f *fakeAccountStore) UpdateInFlight(context.Context, string, int) error {
	panic("unexpected UpdateInFlight")
}
func (f *fakeAccountStore) ResetAllInFlight(context.Context) (int, error) { return 0, nil }
func (f *fakeAccountStore) MarkStatus(context.Context, string, domain.AccountStatus, string) error {
	panic("unexpected MarkStatus")
}
func (f *fakeAccountStore) MarkThrottled(context.Context, string, time.Time, string) error {
	panic("unexpected MarkThrottled")
}
func (f *fakeAccountStore) RecoverThrottled(context.Context) (int, error) { return 0, nil }
func (f *fakeAccountStore) IncrFailStreak(context.Context, string) (int, error) {
	panic("unexpected IncrFailStreak")
}
func (f *fakeAccountStore) ResetFailStreak(context.Context, string) error {
	panic("unexpected ResetFailStreak")
}
func (f *fakeAccountStore) PickAndLock(context.Context, ports.PickParams) (*domain.Account, string, error) {
	panic("unexpected PickAndLock")
}
func (f *fakeAccountStore) UpdateFreeQuota(context.Context, string, domain.FreeQuotaCounters) error {
	return nil
}
func (f *fakeAccountStore) ListUnlimActivations(context.Context, string) ([]domain.UnlimActivation, error) {
	return nil, nil
}
func (f *fakeAccountStore) ReplaceUnlimActivations(context.Context, string, []domain.UnlimActivation) error {
	return nil
}
func (f *fakeAccountStore) Unlock(context.Context, string, string) error {
	panic("unexpected Unlock")
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestMarkCompleted_UpsertsAccount verifies the full happy path:
// storeAdapter reads the registration row, marshals the cookies,
// maps the plan_type, converts credits to hundredths, sets Source
// to "registered", then flips the registrations row.
func TestMarkCompleted_UpsertsAccount(t *testing.T) {
	regs := newFakeRegStore()
	regs.rows[42] = &ports.Registration{
		ID:       42,
		Email:    "new@example.com",
		Password: "SecretPass!42",
		Status:   "running",
		ProxyURL: "socks5://proxy:1080",
	}
	accts := &fakeAccountStore{}
	adapter := &storeAdapter{
		main:     regs,
		accounts: accts,
		log:      testLogger(t),
	}

	result := register.CompletedResult{
		AccountID:  "user_new_1",
		UserID:     "user_new_1",
		SessionID:  "sess_abc",
		UserAgent:  "TestUA/1.0",
		DataDomeID: "dd_client_x",
		PlanType:   "starter",
		Credits:    50.5, // →  5050 hundredths
		Cookies: []register.Cookie{
			{Name: "__session", Value: "cookie_val", Domain: "higgsfield.ai", HTTPOnly: true},
			{Name: "datadome", Value: "dd_cookie", Domain: ".higgsfield.ai"},
		},
	}
	if err := adapter.MarkCompleted(context.Background(), "42", result); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	// registrations row flipped.
	if regs.completedID != 42 || regs.completedAcc != "user_new_1" {
		t.Errorf("registrations row: got id=%d account=%q want 42 / user_new_1",
			regs.completedID, regs.completedAcc)
	}

	// Account.Upsert fired with the right fields.
	if accts.upserted == nil {
		t.Fatal("account was not upserted")
	}
	acc := accts.upserted
	if acc.ID != "user_new_1" {
		t.Errorf("Account.ID = %q want user_new_1", acc.ID)
	}
	if acc.Email != "new@example.com" {
		t.Errorf("Account.Email = %q want new@example.com (preserved from registration row)", acc.Email)
	}
	if acc.Password != "SecretPass!42" {
		t.Errorf("Account.Password not preserved: %q", acc.Password)
	}
	if acc.SessionID != "sess_abc" {
		t.Errorf("Account.SessionID = %q", acc.SessionID)
	}
	if acc.UserAgent != "TestUA/1.0" {
		t.Errorf("Account.UserAgent = %q", acc.UserAgent)
	}
	if acc.DataDomeClientID != "dd_client_x" {
		t.Errorf("Account.DataDomeClientID = %q", acc.DataDomeClientID)
	}
	if acc.PlanType != domain.PlanStarter {
		t.Errorf("Account.PlanType = %q want starter", acc.PlanType)
	}
	if acc.SubscriptionBalance != 5050 {
		t.Errorf("Account.SubscriptionBalance = %d want 5050 (credits*100)", acc.SubscriptionBalance)
	}
	if acc.Status != domain.StatusActive {
		t.Errorf("Account.Status = %q want active", acc.Status)
	}
	if acc.BoundProxyURL != "socks5://proxy:1080" {
		t.Errorf("Account.BoundProxyURL = %q (should preserve reg.ProxyURL)", acc.BoundProxyURL)
	}
	if acc.Source != "registered" {
		t.Errorf("Account.Source = %q want registered", acc.Source)
	}

	// Cookies round-trip through JSON correctly.
	var cookies []register.Cookie
	if err := json.Unmarshal([]byte(acc.CookiesJSON), &cookies); err != nil {
		t.Fatalf("CookiesJSON not valid JSON: %v", err)
	}
	if len(cookies) != 2 {
		t.Fatalf("cookies count = %d want 2", len(cookies))
	}
	if cookies[0].Name != "__session" || !cookies[0].HTTPOnly {
		t.Errorf("cookie[0] = %+v", cookies[0])
	}
}

// TestMarkCompleted_UnknownPlanFallsBackToFree covers the defensive
// mapping: if the driver reports a plan string the domain enum
// doesn't know, the account still enters the pool but at the free
// tier (safest floor) rather than crashing the upsert.
func TestMarkCompleted_UnknownPlanFallsBackToFree(t *testing.T) {
	regs := newFakeRegStore()
	regs.rows[7] = &ports.Registration{ID: 7, Email: "u@x.com", Status: "running"}
	accts := &fakeAccountStore{}
	adapter := &storeAdapter{main: regs, accounts: accts, log: testLogger(t)}

	err := adapter.MarkCompleted(context.Background(), "7", register.CompletedResult{
		AccountID: "user_x",
		PlanType:  "brand_new_tier",
	})
	if err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	if accts.upserted.PlanType != domain.PlanFree {
		t.Errorf("unknown plan = %q, want mapped to free", accts.upserted.PlanType)
	}
}

// TestMarkCompleted_NilAccountStoreLogsAndProceeds proves the
// degrade mode: with no AccountStore wired, MarkCompleted logs a
// warn, skips the upsert, and STILL flips the registrations row.
// Matches the rest of the bridge's "best-effort side-effects"
// contract.
func TestMarkCompleted_NilAccountStoreLogsAndProceeds(t *testing.T) {
	regs := newFakeRegStore()
	regs.rows[9] = &ports.Registration{ID: 9, Email: "u@x.com", Status: "running"}
	adapter := &storeAdapter{main: regs, accounts: nil, log: testLogger(t)}

	err := adapter.MarkCompleted(context.Background(), "9", register.CompletedResult{
		AccountID: "user_9",
		PlanType:  "starter",
	})
	if err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	if regs.completedID != 9 {
		t.Errorf("registrations row not flipped: %+v", regs)
	}
}

// TestMarkCompleted_MissingAccountIDIsError covers the sanity check:
// a driver that returned no account_id can't produce a usable pool
// row, so the whole MarkCompleted returns an error rather than
// silently writing a row with an empty id.
func TestMarkCompleted_MissingAccountIDIsError(t *testing.T) {
	regs := newFakeRegStore()
	regs.rows[11] = &ports.Registration{ID: 11, Email: "u@x.com", Status: "running"}
	accts := &fakeAccountStore{}
	adapter := &storeAdapter{main: regs, accounts: accts, log: testLogger(t)}

	err := adapter.MarkCompleted(context.Background(), "11", register.CompletedResult{
		AccountID: "", // driver returned no id
	})
	if err == nil {
		t.Fatal("expected error for empty account_id")
	}
	if accts.upserted != nil {
		t.Error("Upsert must not fire on empty account_id")
	}
}

// TestMarkCompleted_UpsertFailureBubbles verifies error propagation:
// if AccountStore.Upsert fails (e.g. UNIQUE constraint violation)
// MarkCompleted returns the error and does NOT flip the
// registrations row. Retry semantics thus stay intact.
func TestMarkCompleted_UpsertFailureBubbles(t *testing.T) {
	regs := newFakeRegStore()
	regs.rows[13] = &ports.Registration{ID: 13, Email: "u@x.com", Status: "running"}
	accts := &fakeAccountStore{upsertErr: errors.New("UNIQUE constraint failed")}
	adapter := &storeAdapter{main: regs, accounts: accts, log: testLogger(t)}

	err := adapter.MarkCompleted(context.Background(), "13", register.CompletedResult{
		AccountID: "user_13",
	})
	if err == nil {
		t.Fatal("expected upsert error to bubble")
	}
	if regs.completedID != 0 {
		t.Error("registrations row was flipped despite upsert failure — must not happen")
	}
}
