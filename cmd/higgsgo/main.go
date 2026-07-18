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
	"github.com/greensheep999/higgsgo/internal/adapters/registrar/higgsfield"
	"github.com/greensheep999/higgsgo/internal/adapters/storage/sqlite"
	"github.com/greensheep999/higgsgo/internal/api"
	"github.com/greensheep999/higgsgo/internal/api/cpaplugin"
	"github.com/greensheep999/higgsgo/internal/api/v1"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/core/apikey"
	"github.com/greensheep999/higgsgo/internal/core/jwt"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
	"github.com/greensheep999/higgsgo/internal/core/metering"
	"github.com/greensheep999/higgsgo/internal/core/monthreset"
	"github.com/greensheep999/higgsgo/internal/core/pollworker"
	"github.com/greensheep999/higgsgo/internal/core/proxy"
	"github.com/greensheep999/higgsgo/internal/core/refresher"
	"github.com/greensheep999/higgsgo/internal/core/bearer"
	"github.com/greensheep999/higgsgo/internal/core/failover"
	"github.com/greensheep999/higgsgo/internal/core/regression"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/core/webhook"
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
		accountStore     ports.AccountStore
		jobStore         ports.JobStore
		apiKeyStore      ports.APIKeyStore
		usageStore       ports.UsageEventStore
		groupStore       ports.GroupStore
		modelHealthStore ports.ModelHealthStore
		auditStore       ports.AuditStore
		settingsStore    ports.SettingsStore
		failoverEvents   ports.FailoverEventStore
		failoverOverrs   ports.FailoverOverridesStore
		modelOverrides   ports.ModelOverrideStore
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
		apiKeyStore = sqlite.NewAPIKeyStore(db)
		usageStore = sqlite.NewUsageEventStore(db)
		groupStore = sqlite.NewGroupStore(db)
		modelHealthStore = sqlite.NewModelHealthStore(db)
		auditStore = sqlite.NewAuditStore(db)
		settingsStore = sqlite.NewSettingsStore(db)
		failoverEvents = sqlite.NewFailoverEventStore(db)
		failoverOverrs = sqlite.NewFailoverOverridesStore(db)
		modelOverrides = sqlite.NewModelOverrideStore(db)

		// Seed a default admin key on first boot so the WebUI's
		// Keys list has an operator-facing sk-adm- key waiting when
		// a fresh deployment logs in. The plaintext is logged
		// exactly once — the operator captures it from the boot
		// logs and stashes it in their secret manager.
		if err := seedDefaultAdminKey(ctx, apiKeyStore, logger); err != nil {
			logger.Warn("seed default admin key", slog.String("err", err.Error()))
		}
	case "postgres":
		return errors.New("postgres storage adapter not implemented yet")
	}

	// Reconcile in_flight_jobs on boot. If the previous process
	// crashed or was killed between PickAndLock and Unlock, some
	// rows will have leaked slots that would silently deny picks
	// forever (the hardcoded < 5 cap in PickAndLock stays true).
	// Any real in-flight upstream jobs from before the crash are
	// dead — no goroutine is polling them — so a full reset is
	// safe. See docs/ROADMAP.md P0-2.
	if reset, err := accountStore.ResetAllInFlight(ctx); err != nil {
		logger.Warn("reset in_flight on boot", slog.String("err", err.Error()))
	} else if reset > 0 {
		logger.Warn("cleared leaked in_flight counters on boot",
			slog.Int("accounts_reset", reset))
	}

	// Runtime-mutable admin bearer manager. Loads any DB override on
	// boot and falls back to the TOML value. All /admin/* traffic
	// authenticates via BearerAuth(mgr), so a POST to
	// /admin/settings/bearer/rotate takes effect for new requests
	// immediately (with a 30s grace window for in-flight XHRs — see
	// internal/core/bearer for the guarantees).
	bearerMgr := bearer.New(cfg.Server.AdminBearer, settingsStore, logger)
	if err := bearerMgr.Load(ctx); err != nil {
		return fmt.Errorf("load admin bearer: %w", err)
	}
	logger.Info("admin bearer loaded",
		slog.String("source", string(bearerMgr.CurrentSource())),
		slog.String("last4", bearer.Last4(bearerMgr.Current())),
	)

	// Failover controller: nil when cfg.Failover.Enabled is false so
	// the proxy service / pollworker stay on their pre-013 fast path.
	// FallbackFailLimit shadows the deprecated [pool].fail_streak_threshold
	// so a config that never learned about [failover.consecutive] still
	// gets the same MVP behaviour.
	failoverCtl := failover.New(&cfg.Failover, accountStore, failoverEvents, failoverOverrs, nil, logger)
	if failoverCtl != nil {
		failoverCtl.FallbackFailLimit = cfg.Pool.FailStreakThreshold
		logger.Info("failover controller wired",
			slog.Bool("consecutive_enabled", cfg.Failover.Consecutive.Enabled),
			slog.Bool("throttle_enabled", cfg.Failover.Throttle.Enabled),
			slog.Int("fail_limit", cfg.Failover.Consecutive.FailLimit),
		)
		rec := &failover.Recoverer{Accounts: accountStore, Logger: logger}
		go rec.Run(ctx)
	} else {
		logger.Info("failover controller disabled by config")
	}

	// Model registry (jsonstatic backed by data/reference/verified-models.json).
	regPath := filepath.Join(cfg.Models.DataPath, "verified-models.json")
	extraPath := filepath.Join(cfg.Models.DataPath, "model-specs-extra.json")
	registry, err := jsonstatic.New(jsonstatic.Config{
		Path:           regPath,
		ExtraSpecsPath: extraPath,
	})
	if err != nil {
		return fmt.Errorf("load model registry: %w", err)
	}
	logger.Info("model registry loaded",
		slog.String("path", regPath),
		slog.String("extra_path", extraPath))

	// Wire the persisted operator overrides (migration 015) into the
	// registry, then re-Reload so the first request served post-boot
	// already reflects them. Failure here is non-fatal — the registry
	// keeps its pre-override snapshot and operators can retry via
	// POST /admin/models/reload.
	if modelOverrides != nil {
		registry.SetOverrideProvider(modelOverrides)
		if err := registry.Reload(ctx); err != nil {
			logger.Warn("model registry: reload after wiring overrides failed",
				slog.String("err", err.Error()))
		}
	}

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
	upstreamTimeouts := buildUpstreamTimeouts(cfg, logger)
	upstreamClient := upstream.New(httpClient, minter, upstream.Config{
		Timeouts: upstreamTimeouts,
	})
	for endpoint, d := range upstreamTimeouts {
		logger.Info("upstream timeout configured",
			slog.String("endpoint", endpoint),
			slog.Duration("timeout", d))
	}

	// Prometheus metrics: one Registry, shared with the HTTP middleware,
	// the pool collector goroutine, and the metering recorder. Built
	// early so the recorder can be handed a non-nil *Metrics at
	// construction time.
	metrics := observability.NewMetrics()
	upstreamClient.Metrics = metrics

	// Metering recorder: shared by the sync proxy path and the async
	// pollworker. Both invoke OnJobTerminal at the terminal transition so
	// every completed / failed / refunded / timeout job produces exactly one
	// usage_events row. Recorder tolerates a nil store defensively.
	meter := &metering.Recorder{
		Events:   usageStore,
		APIKeys:  apiKeyStore,
		Accounts: accountStore,
		Logger:   logger,
		Metrics:  metrics,
	}

	// Webhook dispatcher: shared by the sync proxy path and the async
	// pollworker. Fire is non-blocking; delivery + retries + drain-on-close
	// are owned by the Dispatcher. The signing key is read from the env
	// (HIGGSGO_WEBHOOK_SIGNING_KEY) because it is a secret and not yet
	// modelled in the TOML config schema. Empty key disables signing.
	webhooks := webhook.New(logger, webhook.Config{
		SigningKey: os.Getenv("HIGGSGO_WEBHOOK_SIGNING_KEY"),
	})
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelShutdown()
		webhooks.Close(shutdownCtx)
	}()

	svc := &proxy.Service{
		Store:            accountStore,
		Registry:         registry,
		Upstream:         upstreamClient,
		Jobs:             jobStore,
		Groups:           groupStore,
		Logger:           logger,
		Clock:            ports.RealClock{},
		AsyncByDefault:   true,
		SyncPollDeadline: 3 * time.Minute,
		APIKeys:          apiKeyStore,
		Meter:            meter,
		Webhooks:         webhooks,
		Failover:         failoverCtl,
	}
	v1h := v1.New(svc, registry, jobStore, groupStore, apiKeyStore)
	v1h.Logger = logger
	// Enable the pool-side unlim-override probe in /v1/playground/estimate
	// so RequiresUnlim models correctly report will_charge=false when at
	// least one unlim account is live in the pool.
	v1h.Accounts = accountStore

	// Background poll worker: catches slow B-class models that finish
	// after the sync HTTP request has returned. Without this, users would
	// have to poll /v1/jobs/{id} themselves for every job — including the
	// ~30-40 minute wan_animate ones.
	worker := pollworker.Defaults()
	worker.Jobs = jobStore
	worker.Accounts = accountStore
	worker.Upstream = upstreamClient
	worker.Logger = logger
	worker.APIKeys = apiKeyStore
	worker.Meter = meter
	worker.Webhooks = webhooks
	worker.Failover = failoverCtl
	go worker.Run(ctx)

	// Background balance + entitlement refresher: keeps every active
	// account's subscription_balance and plan flags in sync with
	// /workspaces/wallet and /user. Without this, the pool picker drifts
	// (starves an out-of-credits account, misroutes to a downgraded plan).
	refreshInterval, err := time.ParseDuration(cfg.Pool.BalanceRefreshInterval)
	if err != nil || refreshInterval <= 0 {
		logger.Warn("invalid pool.balance_refresh_interval, falling back to 10m",
			slog.String("value", cfg.Pool.BalanceRefreshInterval))
		refreshInterval = 10 * time.Minute
	}
	rf := refresher.New(accountStore, upstreamClient, logger)
	rf.Interval = refreshInterval
	logger.Info("refresher started", slog.Duration("interval", refreshInterval))
	go rf.Run(ctx)

	// Background regression ticker: samples a handful of image models
	// once per Interval and records the outcome to model_health.
	//
	// We ALWAYS construct the Ticker so /admin/tickers/regression can
	// fire a one-shot probe from the WebUI. The [tickers.a_regression].
	// enabled flag only gates the background scheduler — flipping it
	// off keeps dev boots quiet without disabling the manual button.
	// SkipUpstream defaults to true when the schedule is disabled so a
	// misconfig can't drain credits from a manual click.
	interval, err := time.ParseDuration(cfg.Tickers.ARegression.Interval)
	if err != nil || interval <= 0 {
		if cfg.Tickers.ARegression.Enabled {
			logger.Warn("invalid tickers.a_regression.interval, falling back to 24h",
				slog.String("value", cfg.Tickers.ARegression.Interval))
		}
		interval = 24 * time.Hour
	}
	tk := &regression.Ticker{
		Health:       modelHealthStore,
		Registry:     registry,
		Proxy:        svc,
		Logger:       logger,
		Interval:     interval,
		Concurrency:  2,
		SampleSize:   cfg.Tickers.ARegression.SampleSize,
		SkipUpstream: cfg.Tickers.ARegression.SkipUpstream,
	}
	if cfg.Tickers.ARegression.Enabled {
		logger.Info("regression ticker started",
			slog.Duration("interval", interval),
			slog.Int("sample_size", cfg.Tickers.ARegression.SampleSize),
			slog.Bool("skip_upstream", cfg.Tickers.ARegression.SkipUpstream),
		)
		go tk.Run(ctx)
	} else {
		logger.Info("regression ticker scheduler disabled (manual trigger still available)",
			slog.Bool("skip_upstream", cfg.Tickers.ARegression.SkipUpstream),
		)
	}

	// Background monthly usage reset ticker: zeros api_keys.monthly_used
	// at each UTC calendar-month boundary. On by default because
	// monthly_used is a hard quota ceiling and a stale value would
	// silently freeze traffic on the first of the month. An empty or
	// zero interval selects the production calendar path; a positive
	// duration switches to polling mode for local testing.
	if cfg.Tickers.MonthReset.Enabled {
		mr := &monthreset.Ticker{
			APIKeys: apiKeyStore,
			Logger:  logger,
		}
		mode := "calendar"
		if s := cfg.Tickers.MonthReset.Interval; s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				mr.Interval = d
				mode = "polling"
			} else {
				logger.Warn("invalid tickers.month_reset.interval, using calendar mode",
					slog.String("value", s))
			}
		}
		logger.Info("month reset ticker started", slog.String("mode", mode))
		go mr.Run(ctx)
	}

	// Pool collector goroutine: samples AccountsActive / JobsInFlight
	// from the stores on a fixed interval and updates the shared
	// Metrics gauges. Metrics itself was built earlier alongside the
	// metering recorder.
	poolCollector := &observability.PoolCollector{
		Accounts: accountStore,
		Jobs:     jobStore,
		Metrics:  metrics,
		Interval: 15 * time.Second,
		Logger:   logger,
	}
	go poolCollector.Run(ctx)
	logger.Info("pool collector started", slog.Duration("interval", poolCollector.Interval))

	// Mode B (/internal/*): only wired when the CPA plugin mode is
	// enabled. The internal listener itself is gated the same way in
	// api.New, so leaving cpaHandler nil is fine in Mode A.
	var cpaHandler *cpaplugin.Handler
	if cfg.Modes.CPAPlugin {
		cpaHandler = cpaplugin.New(apiKeyStore, accountStore, jobStore, svc, minter, logger)
		logger.Info("cpa plugin handler wired")
	}

	// Boot API server.
	// Registrar (higgsfield signup flow). Build tag "register" swaps
	// higgsfield.NewRegistrar between stub (returns ErrRegistrarDisabled
	// on every method) and the real puppeteer/OTP/captcha skeleton.
	registrar := higgsfield.NewRegistrar(higgsfield.Deps{})

	srv := api.New(cfg, logger, v1h, apiKeyStore, accountStore, jobStore, usageStore, groupStore, metrics, cpaHandler, modelHealthStore, webhooks, rf, tk, auditStore, registry, settingsStore, bearerMgr)
	srv.Registrar = registrar
	// Wire the failover admin surface (assigned as fields rather than
	// added to the already-large api.New signature).
	srv.FailoverEvents = failoverEvents
	srv.FailoverOverrides = failoverOverrs
	srv.FailoverConfig = &cfg.Failover
	srv.ModelOverrides = modelOverrides
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

// buildUpstreamTimeouts translates [upstream.timeouts] duration strings into
// a keyed map for the upstream client. Empty / invalid values fall back to
// per-endpoint defaults tuned for the fnf.higgsfield.ai + clerk.higgsfield.ai
// traffic patterns: 90s for job creation (POSTs may carry base64 image /
// video payloads), 15s for the small GETs (status, wallet, user), and 30s
// for the marginally larger job detail fetch. The transport-level Timeout on
// the underlying utls client acts as an absolute ceiling above these values.
func buildUpstreamTimeouts(cfg *config.Config, logger *slog.Logger) map[string]time.Duration {
	defaults := map[string]time.Duration{
		"create_job":   90 * time.Second,
		"fetch_status": 15 * time.Second,
		"fetch_job":    30 * time.Second,
		"fetch_wallet": 15 * time.Second,
		"fetch_user":   15 * time.Second,
		"default":      30 * time.Second,
	}
	raw := map[string]string{
		"create_job":   cfg.Upstream.Timeouts.CreateJob,
		"fetch_status": cfg.Upstream.Timeouts.FetchStatus,
		"fetch_job":    cfg.Upstream.Timeouts.FetchJob,
		"fetch_wallet": cfg.Upstream.Timeouts.FetchWallet,
		"fetch_user":   cfg.Upstream.Timeouts.FetchUser,
		"default":      cfg.Upstream.Timeouts.Default,
	}
	out := make(map[string]time.Duration, len(defaults))
	for endpoint, defaultD := range defaults {
		s := raw[endpoint]
		if s == "" {
			out[endpoint] = defaultD
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			logger.Warn("invalid upstream timeout, falling back to default",
				slog.String("endpoint", endpoint),
				slog.String("value", s),
				slog.Duration("default", defaultD))
			out[endpoint] = defaultD
			continue
		}
		out[endpoint] = d
	}
	return out
}

// seedDefaultAdminKey ensures a kind=default sk-adm- key exists so
// the Keys page has a starter row on a brand-new deployment. The
// plaintext is only visible in the boot logs — operators are
// expected to capture it there once and stash it in a secret
// manager (rotate to get a new one if lost). If any kind=default
// key already exists, this is a no-op.
func seedDefaultAdminKey(ctx context.Context, store ports.APIKeyStore, logger *slog.Logger) error {
	all, err := store.List(ctx)
	if err != nil {
		return err
	}
	for _, k := range all {
		if k.Kind == domain.APIKeyKindDefault {
			return nil // already seeded
		}
	}
	plaintext, hash, err := apikey.Generate(apikey.KindDefault)
	if err != nil {
		return err
	}
	k := &domain.APIKey{
		ID:              idgen.NewID("key"),
		KeyHash:         hash,
		Name:            "default-admin",
		CreatedBy:       "system",
		Status:          domain.APIKeyStatusActive,
		MonthlyQuota:    0,
		MarkupPct:       1.0,
		CreatedAt:       time.Now().UTC(),
		PlaygroundScope: domain.PlaygroundScopeFull,
		Kind:            domain.APIKeyKindDefault,
		KeyLast4:        apikey.Last4(plaintext),
	}
	if err := store.Create(ctx, k); err != nil {
		return err
	}
	logger.Warn("seeded default admin key — capture the plaintext now, it will not be shown again",
		slog.String("id", k.ID),
		slog.String("plaintext", plaintext),
	)
	return nil
}

// seedDefaultAdminKey ensures a kind=default sk-adm- key exists so
// the Keys page has a starter row on a brand-new deployment. The
// plaintext is only visible in the boot logs — operators are
// expected to capture it there once and stash it in a secret
// manager (rotate to get a new one if lost). If any kind=default
