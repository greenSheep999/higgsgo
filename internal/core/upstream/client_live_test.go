package upstream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/adapters/httpclient/utls"
	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestSeedanceMiniLive runs a real seedance-2-mini generation against
// higgsfield using the plus account fixture. Verifies our JA3, JWT, and
// job orchestration all cohere. Gated on HIGGSGO_TEST_UPSTREAM=1.
func TestSeedanceMiniLive(t *testing.T) {
	if os.Getenv("HIGGSGO_TEST_UPSTREAM") != "1" {
		t.Skip("set HIGGSGO_TEST_UPSTREAM=1 to run live upstream test")
	}

	acc := loadFixtureAccount(t)

	httpClient, err := utls.New(utls.Config{
		Profile:  "chrome_133",
		ProxyURL: os.Getenv("HIGGSGO_TEST_PROXY_URL"),
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("utls: %v", err)
	}
	minter := jwt.New(httpClient, ports.RealClock{}, jwt.Config{})
	up := New(httpClient, minter, Config{})

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// Sanity: wallet endpoint should return real balance.
	wallet, err := up.FetchWallet(ctx, acc)
	if err != nil {
		t.Fatalf("fetch wallet: %v", err)
	}
	if wallet.SubscriptionBalance <= 0 {
		t.Fatalf("subscription_balance non-positive: %+v", wallet)
	}
	t.Logf("wallet: sub=%d workspace=%s", wallet.SubscriptionBalance, wallet.WorkspaceID)

	// Minimal T2V seedance-2-0-mini body. Empirically verified in
	// higgsfield-register (see data/reference/sealed.json).
	body := map[string]any{
		"params": map[string]any{
			"prompt":             "a red apple rotating on a wooden table, cinematic",
			"medias":             []any{},
			"width":              854,
			"height":             480,
			"duration":           15,
			"resolution":         "480p",
			"aspect_ratio":       "16:9",
			"batch_size":         1,
			"generate_audio":     false,
			"multi_shots":        false,
			"multi_shot_mode":    "custom",
			"multi_prompt":       []any{},
			"speedramp":          "auto",
			"reference_elements": []any{},
			"prompt_language":    "en",
			"genre":              "auto",
		},
		"use_unlim":          false,
		"use_seedream_bonus": false,
	}

	created, err := up.CreateJob(ctx, CreateRequest{
		Account:  acc,
		Endpoint: "/jobs/v2/seedance_2_0_mini",
		Body:     body,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("job created: id=%s cost=%d", created.JobID, created.Cost)

	final, err := up.PollUntilTerminal(ctx, acc, created.JobID, PollOptions{
		Deadline: 5 * time.Minute,
		Interval: 5 * time.Second,
		OnTransition: func(status string, remaining time.Duration) {
			t.Logf("  status=%s remaining=%s", status, remaining.Truncate(time.Second))
		},
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	t.Logf("final: status=%s refunded=%v url=%s", final.Status, final.Refunded, final.ResultURL)
	if final.Status != "completed" {
		t.Fatalf("expected completed, got %q", final.Status)
	}
	if final.ResultURL == "" {
		t.Errorf("completed job has no result_url")
	}
}

// loadFixtureAccount reads the plus account JSON from higgsfield-register/output.
func loadFixtureAccount(t *testing.T) *domain.Account {
	t.Helper()
	p := os.Getenv("HIGGSGO_TEST_ACCOUNT_JSON")
	if p == "" {
		abs, _ := filepath.Abs(filepath.Join("..", "..", "..", "..", "higgsfield-register", "output",
			"higgsfield-e7snnrta97_vietnamcashewnuts.store.json"))
		if _, err := os.Stat(abs); err != nil {
			t.Skipf("no fixture at %s; set HIGGSGO_TEST_ACCOUNT_JSON", abs)
		}
		p = abs
	}
	body, err := os.ReadFile(p)
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
		XDataDomeClientID string            `json:"x_datadome_clientid"`
	}
	if err := json.Unmarshal(body, &na); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	cookies, _ := json.Marshal(na.Cookies)
	return &domain.Account{
		ID:               na.UserID,
		Email:            na.Email,
		SessionID:        na.SessionID,
		PlanType:         domain.PlanType(na.PlanType),
		UserAgent:        na.CapturedUserAgent,
		DataDomeClientID: na.XDataDomeClientID,
		CookiesJSON:      string(cookies),
	}
}
