package register

// Verifies that Flow with a Driver wired in (rather than a browser +
// mailbox + captcha) delegates the whole flow to Driver.Register and
// still walks the store state machine correctly. This is the plumbing
// P4-3b needs to work — proves the "one-shot subprocess" model
// integrates cleanly with the pre-existing store state transitions
// before we spend time on the Node driver itself.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"
)

// captureStore records every state transition so the test can
// assert the store saw the right calls in the right order.
type captureStore struct {
	calls []string
	// completed is populated on MarkCompleted so the test can verify
	// the harvested result was piped through.
	completed CompletedResult
	failedMsg string
}

func (s *captureStore) Enqueue(_ context.Context, _ EnqueueRequest) (string, error) {
	return "", nil
}
func (s *captureStore) NextPending(_ context.Context) (*Registration, error) { return nil, nil }
func (s *captureStore) MarkRunning(_ context.Context, id string) error {
	s.calls = append(s.calls, "MarkRunning:"+id)
	return nil
}
func (s *captureStore) MarkOTPWait(_ context.Context, id string) error {
	s.calls = append(s.calls, "MarkOTPWait:"+id)
	return nil
}
func (s *captureStore) MarkCompleted(_ context.Context, id string, r CompletedResult) error {
	s.calls = append(s.calls, "MarkCompleted:"+id)
	s.completed = r
	return nil
}
func (s *captureStore) MarkFailed(_ context.Context, id string, msg string) error {
	s.calls = append(s.calls, "MarkFailed:"+id)
	s.failedMsg = msg
	return nil
}
func (s *captureStore) Get(_ context.Context, id string) (*Registration, error) { return nil, nil }
func (s *captureStore) List(_ context.Context, _ ListFilter) ([]Registration, error) {
	return nil, nil
}
func (s *captureStore) Retry(_ context.Context, id string) error { return nil }

// fakeDriver is a Driver that returns a scripted response so the
// flow test doesn't depend on the mock package.
type fakeDriver struct {
	result CompletedResult
	err    error
}

func (f *fakeDriver) Name() string { return "fake" }
func (f *fakeDriver) Register(_ context.Context, _ RegisterRequest) (CompletedResult, error) {
	return f.result, f.err
}

func testLog(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestFlow_DriverHappyPath verifies the driver path completes cleanly:
// MarkRunning fires before Register, MarkCompleted fires after with
// the driver's harvested result, and MarkFailed/OTPWait are NOT
// touched (those live on the legacy session path).
func TestFlow_DriverHappyPath(t *testing.T) {
	store := &captureStore{}
	driver := &fakeDriver{
		result: CompletedResult{
			AccountID: "acc_x1",
			SessionID: "sess_x1",
			UserAgent: "TestUA/1.0",
			PlanType:  "starter",
		},
	}
	f := NewFlowWithDriver(driver, store, DefaultConfig(), testLog(t))

	err := f.Execute(context.Background(), &Registration{
		ID:    "reg_1",
		Email: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("expected 2 store calls, got %d: %v", len(store.calls), store.calls)
	}
	if store.calls[0] != "MarkRunning:reg_1" {
		t.Errorf("first call = %q want MarkRunning:reg_1", store.calls[0])
	}
	if store.calls[1] != "MarkCompleted:reg_1" {
		t.Errorf("second call = %q want MarkCompleted:reg_1", store.calls[1])
	}
	if store.completed.AccountID != "acc_x1" {
		t.Errorf("completed.AccountID = %q want acc_x1", store.completed.AccountID)
	}
	if store.completed.SessionID != "sess_x1" {
		t.Errorf("completed.SessionID = %q want sess_x1", store.completed.SessionID)
	}
}

// TestFlow_DriverFailurePath verifies MarkFailed fires with the
// driver's error message, MarkCompleted does not.
func TestFlow_DriverFailurePath(t *testing.T) {
	store := &captureStore{}
	driver := &fakeDriver{err: errors.New("captcha timeout")}
	f := NewFlowWithDriver(driver, store, DefaultConfig(), testLog(t))

	err := f.Execute(context.Background(), &Registration{
		ID:    "reg_2",
		Email: "bob@example.com",
	})
	if err == nil {
		t.Fatal("Execute: expected error, got nil")
	}
	if err.Error() != "captcha timeout" {
		t.Errorf("Execute err = %q want 'captcha timeout'", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("expected 2 store calls, got %d: %v", len(store.calls), store.calls)
	}
	if store.calls[1] != "MarkFailed:reg_2" {
		t.Errorf("second call = %q want MarkFailed:reg_2", store.calls[1])
	}
	if store.failedMsg != "captcha timeout" {
		t.Errorf("failedMsg = %q", store.failedMsg)
	}
}

// TestFlow_DriverRespectsContext confirms ctx cancellation cuts
// through the driver call so a shutdown doesn't wait for an in-
// flight registration to finish. Uses a driver that blocks on a
// timer to force the ctx path.
type slowDriver struct{}

func (s *slowDriver) Name() string { return "slow" }
func (s *slowDriver) Register(ctx context.Context, _ RegisterRequest) (CompletedResult, error) {
	select {
	case <-time.After(5 * time.Second):
		return CompletedResult{}, nil
	case <-ctx.Done():
		return CompletedResult{}, ctx.Err()
	}
}

func TestFlow_DriverRespectsContext(t *testing.T) {
	store := &captureStore{}
	f := NewFlowWithDriver(&slowDriver{}, store, DefaultConfig(), testLog(t))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := f.Execute(ctx, &Registration{ID: "reg_3", Email: "c@example.com"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected ctx timeout error")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Execute took %v, expected fast cancellation", elapsed)
	}
	if len(store.calls) != 2 || store.calls[1] != "MarkFailed:reg_3" {
		t.Errorf("expected MarkFailed:reg_3, got %v", store.calls)
	}
}
