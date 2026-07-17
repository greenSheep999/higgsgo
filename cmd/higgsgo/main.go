// higgsgo — main binary.
//
// Usage:
//
//	higgsgo -config /etc/higgsgo/higgsgo.toml
//
// Starts the public /v1 listener, the admin /admin listener, and (if
// modes.cpa_plugin is enabled) the internal /internal listener.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/greensheep999/higgsgo/internal/adapters/httpclient/utls"
	"github.com/greensheep999/higgsgo/internal/adapters/modelregistry/jsonstatic"
	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/api"
	"github.com/greensheep999/higgsgo/internal/api/v1"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/core/pollworker"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/observability"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "higgsgo: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "configs/higgsgo.example.toml", "path to TOML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := observability.NewLogger(cfg.Observability.LogLevel, cfg.Observability.LogFormat)
	logger.Info("higgsgo starting",
		slog.String("config", *configPath),
		slog.String("mode.standalone", boolStr(cfg.Modes.Standalone)),
		slog.String("mode.cpa_plugin", boolStr(cfg.Modes.CPAPlugin)),
	)

	// Open storage. Only sqlite is wired up for now; postgres path exists in
	// config but the adapter package hasn't been written yet.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Storage.
	var (
		accountStore ports.AccountStore
		jobStore     ports.JobStore
	)
	switch cfg.Storage.Driver {
	case "sqlite":
		db, err := sqlite.Open(ctx, cfg.Storage.SQLite.Path)
		if err != nil {
			return fmt.Errorf("open sqlite: %w", err)
		}
		defer db.Close()
		logger.Info("sqlite opened", slog.String("path", db.Path()))
		accountStore = sqlite.NewAccountStore(db)
		jobStore = sqlite.NewJobStore(db)
	case "postgres":
		return errors.New("postgres storage adapter not implemented yet")
	}

	// Model registry (jsonstatic backed by data/reference/verified-models.json).
	regPath := filepath.Join(cfg.Models.DataPath, "verified-models.json")
	registry, err := jsonstatic.New(jsonstatic.Config{Path: regPath})
	if err != nil {
		return fmt.Errorf("load model registry: %w", err)
	}
	logger.Info("model registry loaded", slog.String("path", regPath))

	// Upstream HTTP client (utls Chrome fingerprint).
	proxyURL := os.Getenv("HIGGSGO_UPSTREAM_PROXY_URL")
	httpClient, err := utls.New(utls.Config{
		Profile:  cfg.HTTPClient.UTLS.Profile,
		ProxyURL: proxyURL,
		Timeout:  60 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("build utls client: %w", err)
	}
	logger.Info("upstream http client ready",
		slog.String("fingerprint", httpClient.Fingerprint()),
		slog.String("proxy", proxyURL))

	// Core services.
	minter := jwt.New(httpClient, ports.RealClock{}, jwt.Config{})
	upstreamClient := upstream.New(httpClient, minter, upstream.Config{})
	svc := &proxy.Service{
		Store:            accountStore,
		Registry:         registry,
		Upstream:         upstreamClient,
		Jobs:             jobStore,
		Logger:           logger,
		Clock:            ports.RealClock{},
		AsyncByDefault:   true,
		SyncPollDeadline: 3 * time.Minute,
	}
	v1h := v1.New(svc, registry, jobStore)

	// Background poll worker: catches slow B-class models that finish
	// after the sync HTTP request has returned. Without this, users would
	// have to poll /v1/jobs/{id} themselves for every job — including the
	// ~30-40 minute wan_animate ones.
	worker := pollworker.Defaults()
	worker.Jobs = jobStore
	worker.Accounts = accountStore
	worker.Upstream = upstreamClient
	worker.Logger = logger
	go worker.Run(ctx)

	// Boot API server.
	srv := api.New(cfg, logger, v1h)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	logger.Info("higgsgo stopped")
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
