// Package notifier provides concrete ports.Notifier implementations
// (Slack, stdout) and a Chain that fans out to several destinations with
// a minimum-level filter. The interface (ports.Notifier) and its sole
// consumer (core/failover) already existed; this package is the missing
// implementation that main.go wires in place of the previous nil.
package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// slackNotifier posts notifications to a Slack incoming-webhook URL as a
// plain-text message. It uses a bare net/http client — Slack is a normal
// public POST, not a higgsfield anti-bot endpoint, so the uTLS adapter is
// deliberately not involved.
type slackNotifier struct {
	webhook string
	client  *http.Client
}

func newSlackNotifier(webhook string) *slackNotifier {
	return &slackNotifier{
		webhook: webhook,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *slackNotifier) Name() string { return "slack" }

func (s *slackNotifier) Send(ctx context.Context, msg ports.Notification) error {
	payload := map[string]string{"text": formatText(msg)}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack HTTP %d", resp.StatusCode)
	}
	return nil
}

// formatText renders a Notification into a single Slack-friendly string:
// "[LEVEL] Title\nBody\nkey=value ...". Tags are sorted for stable output.
func formatText(msg ports.Notification) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", strings.ToUpper(string(msg.Level)), msg.Title)
	if msg.Body != "" {
		b.WriteString("\n")
		b.WriteString(msg.Body)
	}
	if len(msg.Tags) > 0 {
		keys := make([]string, 0, len(msg.Tags))
		for k := range msg.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\n")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "%s=%s", k, msg.Tags[k])
		}
	}
	return b.String()
}
