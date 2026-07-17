package jwt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/adapters/httpclient/utls"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestMintLive drives an actual mint against clerk.higgsfield.ai using the
// e7snnrta97 (Plus) account fixture from higgsfield-register/output.
// Gated on HIGGSGO_TEST_UPSTREAM=1.
func TestMintLive(t *testing.T) {
	if os.Getenv("HIGGSGO_TEST_UPSTREAM") != "1" {
		t.Skip("set HIGGSGO_TEST_UPSTREAM=1 to run live jwt mint test")
	}
	fixturePath := os.Getenv("HIGGSGO_TEST_ACCOUNT_JSON")
	if fixturePath == "" {
		// Default to the plus account we know has has_unlim.
		fixturePath = filepath.Join("..", "..", "..", "..", "higgsfield-register", "output",
			"higgsfield-e7snnrta97_vietnamcashewnuts.store.json")
		abs, _ := filepath.Abs(fixturePath)
		if _, err := os.Stat(abs); err != nil {
			t.Skipf("account fixture not found at %s (set HIGGSGO_TEST_ACCOUNT_JSON)", abs)
		}
		fixturePath = abs
	}

	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var na struct {
		Email             string            `json:"email"`
		UserID            string            `json:"user_id"`
		SessionID         string            `json:"session_id"`
		PlanType          string            `json:"plan_type"`
		Cookies           map[string]string `json:"cookies"`
		CapturedUserAgent string            `json:"captured_user_agent"`
	}
	if err := json.Unmarshal(body, &na); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	acc := &domain.Account{
		ID:        na.UserID,
		Email:     na.Email,
		SessionID: na.SessionID,
		PlanType:  domain.PlanType(na.PlanType),
		UserAgent: na.CapturedUserAgent,
	}
	cookiesJSON, _ := json.Marshal(na.Cookies)
	acc.CookiesJSON = string(cookiesJSON)

	client, err := utls.New(utls.Config{
		Profile:  "chrome_133",
		ProxyURL: os.Getenv("HIGGSGO_TEST_PROXY_URL"),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("utls client: %v", err)
	}

	m := New(client, ports.RealClock{}, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tok, err := m.Get(ctx, acc)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok.JWT == "" {
		t.Fatal("empty jwt")
	}
	if tok.Claims.Sub != acc.ID {
		t.Errorf("jwt sub mismatch: got %q want %q", tok.Claims.Sub, acc.ID)
	}
	t.Logf("minted jwt: sub=%s exp=%s email=%s", tok.Claims.Sub, tok.Expiry().Format(time.RFC3339), tok.Claims.Email)

	// Second Get should hit the cache.
	tok2, err := m.Get(ctx, acc)
	if err != nil {
		t.Fatalf("mint 2: %v", err)
	}
	if tok2.JWT != tok.JWT {
		t.Error("expected cached jwt on second Get")
	}
}
