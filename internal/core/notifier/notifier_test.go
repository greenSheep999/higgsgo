package notifier

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recNotifier records what it received, for chain tests.
type recNotifier struct {
	mu   sync.Mutex
	msgs []ports.Notification
	err  error
}

func (r *recNotifier) Name() string { return "rec" }
func (r *recNotifier) Send(_ context.Context, m ports.Notification) error {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	return r.err
}

func TestSlackNotifier_PostsFormattedText(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newSlackNotifier(srv.URL)
	err := s.Send(context.Background(), ports.Notification{
		Level: ports.LevelWarn, Title: "Pool low", Body: "top up",
		Tags: map[string]string{"jst": "seedance_2_unlimited"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(got, "WARN") || !strings.Contains(got, "Pool low") || !strings.Contains(got, "seedance_2_unlimited") {
		t.Errorf("payload missing expected content: %s", got)
	}
}

func TestSlackNotifier_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := newSlackNotifier(srv.URL).Send(context.Background(), ports.Notification{Title: "x"}); err == nil {
		t.Fatal("expected error on 500")
	}
}

func TestChain_MinLevelFilters(t *testing.T) {
	rec := &recNotifier{}
	chain := &Chain{notifiers: []ports.Notifier{rec}, minLevel: ports.LevelWarn, logger: discardLogger()}

	// Info is below Warn — dropped.
	_ = chain.Send(context.Background(), ports.Notification{Level: ports.LevelInfo, Title: "info"})
	// Error is above Warn — delivered.
	_ = chain.Send(context.Background(), ports.Notification{Level: ports.LevelError, Title: "err"})

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.msgs) != 1 || rec.msgs[0].Title != "err" {
		t.Fatalf("expected only the error message, got %+v", rec.msgs)
	}
}

func TestChain_OneFailureDoesNotBlockOthers(t *testing.T) {
	failing := &recNotifier{err: io.ErrUnexpectedEOF}
	ok := &recNotifier{}
	chain := &Chain{notifiers: []ports.Notifier{failing, ok}, minLevel: ports.LevelInfo, logger: discardLogger()}

	err := chain.Send(context.Background(), ports.Notification{Level: ports.LevelWarn, Title: "x"})
	if err == nil {
		t.Error("expected first error to surface")
	}
	ok.mu.Lock()
	defer ok.mu.Unlock()
	if len(ok.msgs) != 1 {
		t.Errorf("second notifier should still receive the message, got %d", len(ok.msgs))
	}
}

func TestNew_EmptyConfigFallsBackToStdout(t *testing.T) {
	chain := New(nil, discardLogger())
	if len(chain.notifiers) != 1 || chain.notifiers[0].Name() != "stdout" {
		t.Fatalf("expected stdout-only chain, got %d notifiers", len(chain.notifiers))
	}
}

func TestNew_SlackWithoutWebhookSkipped(t *testing.T) {
	chain := New([]config.NotifierConfig{{Type: "slack"}}, discardLogger())
	// slack skipped (no webhook) → falls back to stdout
	if len(chain.notifiers) != 1 || chain.notifiers[0].Name() != "stdout" {
		t.Fatalf("expected stdout fallback, got %+v", chain.notifiers)
	}
}

func TestNew_SlackConfigured(t *testing.T) {
	chain := New([]config.NotifierConfig{{Type: "slack", MinLevel: "warn", Slack: config.SlackNotifierConfig{Webhook: "https://hooks.example/x"}}}, discardLogger())
	if len(chain.notifiers) != 1 || chain.notifiers[0].Name() != "slack" {
		t.Fatalf("expected slack notifier, got %+v", chain.notifiers)
	}
	if chain.minLevel != ports.LevelWarn {
		t.Errorf("minLevel: got %q want warn", chain.minLevel)
	}
}
