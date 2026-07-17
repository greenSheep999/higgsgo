// import-node-accounts reads higgsfield-*.json files produced by the Node
// registration project and imports them into the higgsgo SQLite pool.
//
// Usage:
//
//	go run ./scripts/import-node-accounts -config configs/higgsgo.example.toml \
//	    -dir /path/to/higgsfield-register/output
//
// Each JSON file is expected to have the shape written by
// higgsfield-register/src/output/writer.mjs:
//
//	{
//	  "type": "higgsfield",
//	  "email": ..., "password": ...,
//	  "user_id": ..., "session_id": ...,
//	  "plan_type": ..., "cookies": {...},
//	  "captured_user_agent": ...,
//	  "x_datadome_clientid": ...,
//	  "imported_at": "...",
//	  "credits_snapshot": {
//	    "subscription_credits": 1000.0,
//	    "package_credits": 10.0,
//	    ...
//	  }
//	}
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/observability"
)

// nodeAccount mirrors the JSON schema emitted by the Node registrar.
// Only fields we actually need are declared; the rest are ignored by
// the json package.
type nodeAccount struct {
	Type              string            `json:"type"`
	Email             string            `json:"email"`
	Password          string            `json:"password"`
	UserID            string            `json:"user_id"`
	SessionID         string            `json:"session_id"`
	PlanType          string            `json:"plan_type"`
	Cookies           map[string]string `json:"cookies"`
	XDataDomeClientID string            `json:"x_datadome_clientid"`
	CapturedUserAgent string            `json:"captured_user_agent"`
	ImportedAt        string            `json:"imported_at"`
	CreditsSnapshot   struct {
		SubscriptionCredits float64 `json:"subscription_credits"`
		PackageCredits      float64 `json:"package_credits"`
		DailyCredits        float64 `json:"daily_credits"`
		TotalPlanCredits    float64 `json:"total_plan_credits"`
		CapturedAt          string  `json:"captured_at"`
	} `json:"credits_snapshot"`
}

func main() {
	configPath := flag.String("config", "configs/higgsgo.example.toml", "config file")
	dir := flag.String("dir", "", "directory containing higgsfield-*.json files (required)")
	dryRun := flag.Bool("dry-run", false, "parse and validate but do not write to DB")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "-dir is required")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	must(err, "load config")

	logger := observability.NewLogger(cfg.Observability.LogLevel, cfg.Observability.LogFormat)
	ctx := context.Background()

	db, err := sqlite.Open(ctx, cfg.Storage.SQLite.Path)
	must(err, "open sqlite")
	defer db.Close()
	store := sqlite.NewAccountStore(db)

	entries, err := os.ReadDir(*dir)
	must(err, "read dir")

	var (
		total, imported, skipped int
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "higgsfield-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		total++
		path := filepath.Join(*dir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			logger.Warn("read file", slog.String("path", path), slog.String("err", err.Error()))
			skipped++
			continue
		}
		var na nodeAccount
		if err := json.Unmarshal(body, &na); err != nil {
			logger.Warn("parse json", slog.String("path", path), slog.String("err", err.Error()))
			skipped++
			continue
		}
		if na.Type != "higgsfield" {
			logger.Warn("skip non-higgsfield json", slog.String("path", path), slog.String("type", na.Type))
			skipped++
			continue
		}
		if na.UserID == "" || na.Email == "" {
			logger.Warn("skip malformed", slog.String("path", path))
			skipped++
			continue
		}

		acct := nodeAccountToDomain(&na, logger)
		if *dryRun {
			logger.Info("dry-run",
				slog.String("email", acct.Email),
				slog.String("plan", string(acct.PlanType)),
				slog.Int64("sub_bal", acct.SubscriptionBalance),
			)
			imported++
			continue
		}
		if err := store.Upsert(ctx, acct); err != nil {
			logger.Error("upsert", slog.String("email", acct.Email), slog.String("err", err.Error()))
			skipped++
			continue
		}
		imported++
		logger.Info("imported",
			slog.String("id", acct.ID),
			slog.String("email", acct.Email),
			slog.String("plan", string(acct.PlanType)),
			slog.Int64("sub_bal", acct.SubscriptionBalance),
		)
	}
	logger.Info("done",
		slog.Int("total_files", total),
		slog.Int("imported", imported),
		slog.Int("skipped", skipped),
	)
}

// nodeAccountToDomain converts the Node JSON shape into a domain.Account.
func nodeAccountToDomain(na *nodeAccount, logger *slog.Logger) *domain.Account {
	cookiesJSON, err := json.Marshal(na.Cookies)
	if err != nil {
		cookiesJSON = []byte("{}")
	}
	// Credits are stored in cents (credits * 100) to match higgsfield's
	// internal representation, avoiding float drift.
	subH := int64(na.CreditsSnapshot.SubscriptionCredits * 100)
	pkgH := int64(na.CreditsSnapshot.PackageCredits * 100)
	totalH := int64(na.CreditsSnapshot.TotalPlanCredits * 100)

	// Parse timestamps.
	importedAt := parseNodeTime(na.ImportedAt)
	if importedAt.IsZero() {
		importedAt = time.Now().UTC()
	}
	regAt := parseNodeTime(na.CreditsSnapshot.CapturedAt)
	if regAt.IsZero() {
		regAt = importedAt
	}

	return &domain.Account{
		ID:                  na.UserID,
		Email:               na.Email,
		Password:            na.Password, // NOTE: stored plaintext for now; encrypt at rest is TODO.
		SessionID:           na.SessionID,
		CookiesJSON:         string(cookiesJSON),
		UserAgent:           na.CapturedUserAgent,
		DataDomeClientID:    na.XDataDomeClientID,
		PlanType:            domain.PlanType(na.PlanType),
		SubscriptionBalance: subH,
		CreditsBalance:      pkgH,
		TotalPlanCredits:    totalH,
		Status:              domain.StatusActive,
		RegisteredAt:        regAt,
		ImportedAt:          importedAt,
	}
}

func parseNodeTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func must(err error, msg string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
		os.Exit(1)
	}
}
