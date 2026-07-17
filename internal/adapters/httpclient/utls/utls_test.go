package utls

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestReachesHiggsfield verifies the utls client can establish TLS with
// higgsfield.ai (which is fronted by Cloudflare and rejects the stdlib
// crypto/tls fingerprint). Skipped unless the env var
// HIGGSGO_TEST_UPSTREAM=1 is set — this is a live-network test.
func TestReachesHiggsfield(t *testing.T) {
	if os.Getenv("HIGGSGO_TEST_UPSTREAM") != "1" {
		t.Skip("set HIGGSGO_TEST_UPSTREAM=1 to run live upstream tests")
	}

	proxyURL := os.Getenv("HIGGSGO_TEST_PROXY_URL") // e.g. socks5://127.0.0.1:10808
	c, err := New(Config{Profile: "chrome_133", ProxyURL: proxyURL, Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://fnf.higgsfield.ai/subscriptions/plans", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")

	resp, err := c.Do(ctx, req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("upstream returned 403 — TLS fingerprint likely rejected. status=%d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		t.Logf("upstream returned status=%d (non-fatal for auth-required endpoints)", resp.StatusCode)
	}
}
