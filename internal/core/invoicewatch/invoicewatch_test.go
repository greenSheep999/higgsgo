package invoicewatch

// Tests for the invoicewatch tick. A real upstream.Client is pointed at an
// httptest server that records retry-payment POSTs; a fake store returns
// active accounts, and a fake notifier records alerts. Assertions cover:
// pending invoice → retry POST fired; empty pending → no retry; retry budget
// exhausted (>3 in 24h) with invoices still pending → Warn alert fired once.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// fakeStore embeds ports.AccountStore (nil) and overrides only List — the
// watcher touches nothing else, so a call to any other method would panic,
// which is the desired tripwire if the deps ever grow silently.
type fakeStore struct {
	ports.AccountStore
	accounts []domain.Account
}

func (f *fakeStore) List(context.Context, ports.AccountFilter) ([]domain.Account, error) {
	return f.accounts, nil
}

// fakeNotifier records sent notifications.
type fakeNotifier struct {
	mu   sync.Mutex
	sent []ports.Notification
}

func (f *fakeNotifier) Name() string { return "fake" }
func (f *fakeNotifier) Send(_ context.Context, m ports.Notification) error {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return nil
}
func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// fakeHTTPClient short-circuits the clerk JWT mint and forwards everything
// else to the real transport (so the httptest server serves the invoice /
// retry endpoints).
type fakeHTTPClient struct{ mintJWT string }

func (f *fakeHTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.URL.Host == "clerk.higgsfield.ai" {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf(`{"jwt":%q}`, f.mintJWT))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return http.DefaultClient.Do(req.WithContext(ctx))
}
func (f *fakeHTTPClient) Fingerprint() string { return "fake" }
func (f *fakeHTTPClient) Name() string        { return "fake" }

func newFakeJWT() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"sub": "user_test", "email": "t@example.com",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
	})
	return header + "." + base64.RawURLEncoding.EncodeToString(claims) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

func mkAccount(id string) domain.Account {
	return domain.Account{
		ID: id, Email: id + "@example.com", SessionID: "sess_" + id,
		CookiesJSON: `{"__session":"stub"}`, UserAgent: "test", Status: domain.StatusActive,
	}
}

func newWatcher(srv *httptest.Server, store ports.AccountStore, ntf ports.Notifier) *Watcher {
	fake := &fakeHTTPClient{mintJWT: newFakeJWT()}
	minter := jwt.New(fake, ports.RealClock{}, jwt.Config{})
	up := upstream.New(fake, minter, upstream.Config{BaseURL: srv.URL})
	w := New(store, up, ntf, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.Concurrency = 1
	return w
}

// recorder captures the retry POSTs the tick fires.
type recorder struct {
	mu         sync.Mutex
	retryPosts []string // account "user_test" hits — count by call
}

func (rec *recorder) countRetries() int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return len(rec.retryPosts)
}

// mkServer serves the given pending-invoices JSON on GET and records retry
// POSTs into rec.
func mkServer(rec *recorder, invoicesJSON string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/workspaces/pending-invoices":
			_, _ = w.Write([]byte(invoicesJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/auto-top-ups/retry-payment":
			rec.mu.Lock()
			rec.retryPosts = append(rec.retryPosts, r.URL.Path)
			rec.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func TestInvoiceWatch_PendingInvoiceTriggersRetry(t *testing.T) {
	rec := &recorder{}
	invoices := `{"items":[{"id":"inv_a","status":"open","amount":"1000","currency":"usd"}],"cursor":null}`
	srv := mkServer(rec, invoices)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}, ntf)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w.TriggerOnce(ctx)

	if got := rec.countRetries(); got != 1 {
		t.Fatalf("expected 1 retry-payment POST, got %d", got)
	}
	if got := ntf.count(); got != 0 {
		t.Errorf("expected no alert on first retry, got %d", got)
	}
}

func TestInvoiceWatch_EmptyPendingNoRetry(t *testing.T) {
	rec := &recorder{}
	srv := mkServer(rec, `{"items":[],"cursor":null}`)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}, ntf)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w.TriggerOnce(ctx)

	if got := rec.countRetries(); got != 0 {
		t.Fatalf("empty pending should trigger no retry, got %d", got)
	}
	if got := ntf.count(); got != 0 {
		t.Errorf("empty pending should trigger no alert, got %d", got)
	}
}

func TestInvoiceWatch_ExhaustedBudgetAlerts(t *testing.T) {
	rec := &recorder{}
	// Invoices stay pending across every tick, so retries keep being
	// attempted until the 24h budget (3) is spent, then an alert fires.
	invoices := `{"items":[{"id":"inv_a","status":"open"}],"cursor":null}`
	srv := mkServer(rec, invoices)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}, ntf)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 5 ticks within the same window: 3 retries, then 2 exhausted passes.
	for i := 0; i < 5; i++ {
		w.TriggerOnce(ctx)
	}

	if got := rec.countRetries(); got != maxRetriesPerWindow {
		t.Fatalf("expected exactly %d retries in the window, got %d", maxRetriesPerWindow, got)
	}
	if got := ntf.count(); got != 1 {
		t.Fatalf("expected exactly 1 exhausted-budget alert (deduped), got %d", got)
	}
	ntf.mu.Lock()
	alert := ntf.sent[0]
	ntf.mu.Unlock()
	if alert.Level != ports.LevelWarn {
		t.Errorf("alert level: got %q want warn", alert.Level)
	}
	if alert.Tags["account_id"] != "acc_1" {
		t.Errorf("alert account_id: got %q want acc_1", alert.Tags["account_id"])
	}
}

func TestInvoiceWatch_WindowResetReArmsRetries(t *testing.T) {
	rec := &recorder{}
	invoices := `{"items":[{"id":"inv_a","status":"open"}],"cursor":null}`
	srv := mkServer(rec, invoices)
	defer srv.Close()

	ntf := &fakeNotifier{}
	w := newWatcher(srv, &fakeStore{accounts: []domain.Account{mkAccount("acc_1")}}, ntf)
	// Controllable clock: start at t0.
	base := time.Now()
	var mu sync.Mutex
	cur := base
	w.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return cur }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Spend the budget (3 retries) in window 1.
	for i := 0; i < 3; i++ {
		w.TriggerOnce(ctx)
	}
	if got := rec.countRetries(); got != maxRetriesPerWindow {
		t.Fatalf("window 1: expected %d retries, got %d", maxRetriesPerWindow, got)
	}

	// Advance past the 24h window — budget re-arms.
	mu.Lock()
	cur = base.Add(retryWindow + time.Minute)
	mu.Unlock()
	w.TriggerOnce(ctx)

	if got := rec.countRetries(); got != maxRetriesPerWindow+1 {
		t.Fatalf("window 2: expected a fresh retry (%d total), got %d", maxRetriesPerWindow+1, got)
	}
}
