// Package config loads higgsgo settings from a TOML file and environment
// variables. See configs/higgsgo.example.toml for the full schema.
//
// Environment variable overrides use the prefix HIGGSGO_ and dot-to-underscore
// mapping (e.g., HIGGSGO_SERVER_LISTEN overrides [server].listen).
package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// Config is the root configuration struct.
type Config struct {
	Server        ServerConfig        `toml:"server"`
	Storage       StorageConfig       `toml:"storage"`
	HTTPClient    HTTPClientConfig    `toml:"http_client"`
	Upstream      UpstreamConfig      `toml:"upstream"`
	Proxy         ProxyConfig         `toml:"proxy"`
	Mailbox       []MailboxConfig     `toml:"mailbox"`
	Captcha       CaptchaConfig       `toml:"captcha"`
	Browser       BrowserConfig       `toml:"browser"`
	Models        ModelsConfig        `toml:"models"`
	Pool          PoolConfig          `toml:"pool"`
	Register      RegisterConfig      `toml:"register"`
	Modes         ModesConfig         `toml:"modes"`
	Tickers       TickersConfig       `toml:"tickers"`
	Notifiers     []NotifierConfig    `toml:"notifiers"`
	Observability ObservabilityConfig `toml:"observability"`
	Updates       UpdatesConfig       `toml:"updates"`
	Failover      FailoverConfig      `toml:"failover"`
}

// FailoverConfig configures the two automatic-isolation mechanisms
// implemented in core/failover. When disabled the pool behaves exactly
// like it did pre-013 (no auto-disable, no throttle cooldowns).
type FailoverConfig struct {
	Enabled     bool                       `toml:"enabled"`
	Consecutive ConsecutiveFailoverConfig  `toml:"consecutive"`
	Throttle    ThrottleFailoverConfig     `toml:"throttle"`
	OutageGuard OutageGuardConfig          `toml:"outage_guard"`
}

// ConsecutiveFailoverConfig — mechanism ①: N account-attributable
// failures in a row → disable.
type ConsecutiveFailoverConfig struct {
	Enabled   bool `toml:"enabled"`
	FailLimit int  `toml:"fail_limit"`
}

// ThrottleFailoverConfig — mechanism ②: sliding window over 429 /
// risk-marker events. Off by default until real production 429 body
// samples let us populate RiskMarkers safely.
type ThrottleFailoverConfig struct {
	Enabled        bool     `toml:"enabled"`
	JudgeWindowSec int      `toml:"judge_window_sec"`
	JudgeCount     int      `toml:"judge_count"`
	CooldownSec    int      `toml:"cooldown_sec"`
	EvictWindowSec int      `toml:"evict_window_sec"`
	EvictCount     int      `toml:"evict_count"`
	// RiskMarkers is a case-insensitive substring list matched against
	// 429 response bodies. Empty = every 429 counts equally. TODO
	// (failover): fill this in after collecting real higgsfield 429 /
	// DataDome bodies. Do not use third-party CDN/WAF product names.
	RiskMarkers []string `toml:"risk_markers"`
}

// OutageGuardConfig — pool-level circuit breaker. If the controller
// disabled more than DisableCountLimit accounts in the last WindowSec
// seconds, stop disabling and just log/alert (assume it's a
// higgsfield-wide incident, not N bad accounts).
type OutageGuardConfig struct {
	WindowSec         int `toml:"window_sec"`
	DisableCountLimit int `toml:"disable_count_limit"`
}

// UpdatesConfig controls the /admin/version/check endpoint that polls
// GitHub Releases for a newer higgsgo. Purely advisory — the running
// binary is never replaced; the WebUI simply surfaces a badge when
// a newer release is available.
//
// Owner is case-sensitive at api.github.com (mismatched case still
// resolves to the same repo but leaks the mismatch back in error
// bodies), so keep the canonical mixed-case here.
type UpdatesConfig struct {
	GitHubOwner  string `toml:"github_owner"`
	GitHubRepo   string `toml:"github_repo"`
	CheckEnabled bool   `toml:"check_enabled"`
}

type ServerConfig struct {
	Listen         string `toml:"listen"`
	AdminListen    string `toml:"admin_listen"`
	InternalListen string `toml:"internal_listen"`
	PublicURL      string `toml:"public_url"`
	AdminBearer    string `toml:"admin_bearer"`    // shared secret for /admin/*
	InternalBearer string `toml:"internal_bearer"` // shared secret for /internal/* (CPA plugin)

	RateLimit RateLimitConfig `toml:"rate_limit"`
	WebUI     WebUIConfig     `toml:"webui"`
}

// WebUIConfig controls the CORS allowlist applied on /admin/* so a
// separately-deployed WebUI can reach the admin surface cross-origin.
// Empty Origins keeps the middleware disabled and turns it into a
// pass-through — the same behaviour a server without CORS mounted at
// all would exhibit.
type WebUIConfig struct {
	// Origins is the CORS allowlist. Empty disables CORS.
	// Use ["http://localhost:5173"] for local dev, ["*"] for
	// "any origin" (dangerous, dev only).
	Origins []string `toml:"origins"`
}

// RateLimitConfig controls the per-API-key token bucket applied on /v1/*
// after authentication succeeds. Zero values fall back to the middleware's
// safe defaults (see internal/api/middleware/ratelimit.go).
type RateLimitConfig struct {
	RPS   float64 `toml:"rps"`
	Burst int     `toml:"burst"`
}

type StorageConfig struct {
	Driver   string         `toml:"driver"` // "sqlite" | "postgres"
	SQLite   SQLiteConfig   `toml:"sqlite"`
	Postgres PostgresConfig `toml:"postgres"`
}

type SQLiteConfig struct {
	Path string `toml:"path"`
}

type PostgresConfig struct {
	DSN string `toml:"dsn"`
}

type HTTPClientConfig struct {
	Type string           `toml:"type"` // "utls" | "impit_bridge" | "stdhttp"
	UTLS UTLSClientConfig `toml:"utls"`
}

type UTLSClientConfig struct {
	Profile string `toml:"profile"` // e.g. "chrome_133"
}

// UpstreamConfig groups tunables for the fnf.higgsfield.ai / clerk.higgsfield.ai
// client. Currently only per-endpoint request timeouts live here; the base URL
// and TLS profile stay under [http_client] because they belong to the transport
// layer.
type UpstreamConfig struct {
	Timeouts UpstreamTimeoutsConfig `toml:"timeouts"`
}

// UpstreamTimeoutsConfig sets per-endpoint request timeouts for the upstream
// client. Each value is a Go duration string (e.g. "15s", "90s"). Empty
// values fall back to the built-in defaults set in cmd/higgsgo/main.go
// (create_job=90s, fetch_status/fetch_wallet/fetch_user=15s, fetch_job=30s,
// default=30s).
//
// Endpoint labels match the low-cardinality strings the upstream client
// uses for its Prometheus histogram (see internal/core/upstream/client.go).
type UpstreamTimeoutsConfig struct {
	CreateJob   string `toml:"create_job"`
	FetchStatus string `toml:"fetch_status"`
	FetchJob    string `toml:"fetch_job"`
	FetchWallet string `toml:"fetch_wallet"`
	FetchUser   string `toml:"fetch_user"`
	Default     string `toml:"default"`
}

type ProxyConfig struct {
	Provider string            `toml:"provider"` // "static" | "711proxy" | "brightdata" | "noop"
	Static   StaticProxyConfig `toml:"static"`
}

type StaticProxyConfig struct {
	File string   `toml:"file"`
	URLs []string `toml:"urls"`
}

type MailboxConfig struct {
	Type    string               `toml:"type"` // "graph" | "destiny" | "prompt" | "imap"
	Graph   GraphMailboxConfig   `toml:"graph"`
	Destiny DestinyMailboxConfig `toml:"destiny"`
}

type GraphMailboxConfig struct {
	ListFile string `toml:"list_file"`
}

type DestinyMailboxConfig struct {
	WebURL           string   `toml:"web_url"`
	SupportedDomains []string `toml:"supported_domains"`
}

type CaptchaConfig struct {
	Provider  string          `toml:"provider"` // "capsolver" | "twocaptcha" | "manual"
	CapSolver CapSolverConfig `toml:"capsolver"`
}

type CapSolverConfig struct {
	APIKey         string `toml:"api_key"`
	EnableDataDome bool   `toml:"enable_datadome"`
}

type BrowserConfig struct {
	Type        string                   `toml:"type"` // "cloak_nodejs" | "chromedp"
	CloakNodeJS CloakNodeJSBrowserConfig `toml:"cloak_nodejs"`
}

type CloakNodeJSBrowserConfig struct {
	NodeBin      string `toml:"node_bin"`
	WorkerScript string `toml:"worker_script"`
	PoolSize     int    `toml:"pool_size"`
}

type ModelsConfig struct {
	Path           string `toml:"path"`      // dir with per-family TOML shards
	DataPath       string `toml:"data_path"` // data/reference dir with sealed.json etc.
	ReloadOnSignal string `toml:"reload_on_signal"`
}

type PoolConfig struct {
	MaxInFlightPerAccount  int    `toml:"max_in_flight_per_account"`
	FailStreakThreshold    int    `toml:"fail_streak_threshold"`
	BalanceRefreshInterval string `toml:"balance_refresh_interval"`
	JWTRefreshInterval     string `toml:"jwt_refresh_interval"`
}

type RegisterConfig struct {
	AutoTopup                  bool   `toml:"auto_topup"`
	MinStarterAccounts         int    `toml:"min_starter_accounts"`
	MailListFile               string `toml:"mail_list_file"`
	MaxConcurrentRegistrations int    `toml:"max_concurrent_registrations"`
}

type ModesConfig struct {
	Standalone bool `toml:"standalone"`
	CPAPlugin  bool `toml:"cpa_plugin"`
}

type TickersConfig struct {
	ARegression TickerJobConfig  `toml:"a_regression"`
	X1Recheck   TickerJobConfig  `toml:"x1_recheck"`
	BodyDrift   TickerJobConfig  `toml:"body_drift"`
	MonthReset  MonthResetConfig `toml:"month_reset"`
}

// MonthResetConfig controls the background ticker that zeros every api
// key's monthly_used counter at each calendar month boundary. Interval
// is a Go duration string that forces a polling-loop cadence for tests
// (e.g. "1s"); leaving it empty selects the production, calendar-driven
// path where the ticker sleeps until the next month starts (see
// internal/core/monthreset).
type MonthResetConfig struct {
	Enabled  bool   `toml:"enabled"`
	Interval string `toml:"interval"`
}

type TickerJobConfig struct {
	Cron         string `toml:"cron"`
	Interval     string `toml:"interval"` // Go duration form, e.g. "24h"
	SampleSize   int    `toml:"sample_size"`
	Enabled      bool   `toml:"enabled"`
	SkipUpstream bool   `toml:"skip_upstream"` // dev/test: record probes without hitting proxy
}

type NotifierConfig struct {
	Type     string              `toml:"type"` // "slack" | "telegram" | "webhook" | "email" | "stdout"
	MinLevel string              `toml:"min_level"`
	Slack    SlackNotifierConfig `toml:"slack"`
}

type SlackNotifierConfig struct {
	Webhook string `toml:"webhook"`
}

type ObservabilityConfig struct {
	LogLevel    string `toml:"log_level"`
	LogFormat   string `toml:"log_format"` // "json" | "text"
	MetricsPath string `toml:"metrics_path"`
}

// Load reads a TOML file, applies environment overrides, and validates.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config path required")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	c := defaults()
	if err := toml.Unmarshal(body, c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	c.applyEnv()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return c, nil
}

// defaults returns a Config populated with safe defaults. TOML values override.
func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:      "0.0.0.0:8080",
			AdminListen: "127.0.0.1:8081",
			RateLimit:   RateLimitConfig{RPS: 5, Burst: 10},
		},
		Storage: StorageConfig{
			Driver: "sqlite",
			SQLite: SQLiteConfig{Path: "./data/higgsgo.db"},
		},
		HTTPClient: HTTPClientConfig{
			Type: "stdhttp",
			UTLS: UTLSClientConfig{Profile: "chrome_133"},
		},
		Proxy: ProxyConfig{Provider: "noop"},
		Models: ModelsConfig{
			Path:     "./configs/models",
			DataPath: "./data/reference",
		},
		Pool: PoolConfig{
			MaxInFlightPerAccount:  5,
			FailStreakThreshold:    3,
			BalanceRefreshInterval: "10m",
			JWTRefreshInterval:     "40s",
		},
		Modes: ModesConfig{
			Standalone: true,
			CPAPlugin:  false,
		},
		Observability: ObservabilityConfig{
			LogLevel:    "info",
			LogFormat:   "json",
			MetricsPath: "/metrics",
		},
		Tickers: TickersConfig{
			// Monthly usage reset is on by default because monthly_used
			// is a hard cap on outbound spend and a stale value would
			// silently freeze traffic on the first of the month.
			MonthReset: MonthResetConfig{Enabled: true},
		},
		Updates: UpdatesConfig{
			GitHubOwner:  "greenSheep999",
			GitHubRepo:   "higgsgo",
			CheckEnabled: true,
		},
		Failover: FailoverConfig{
			Enabled: true,
			Consecutive: ConsecutiveFailoverConfig{
				Enabled:   true,
				FailLimit: 3,
			},
			Throttle: ThrottleFailoverConfig{
				Enabled:        false,
				JudgeWindowSec: 60,
				JudgeCount:     5,
				CooldownSec:    600,
				EvictWindowSec: 3600,
				EvictCount:     3,
				RiskMarkers:    []string{},
			},
			OutageGuard: OutageGuardConfig{
				WindowSec:         30,
				DisableCountLimit: 3,
			},
		},
	}
}

// applyEnv overlays HIGGSGO_* environment variables. Kept minimal for now;
// callers can extend as they add config keys that must be settable at runtime
// (e.g., secrets).
func (c *Config) applyEnv() {
	if v := os.Getenv("HIGGSGO_SERVER_LISTEN"); v != "" {
		c.Server.Listen = v
	}
	if v := os.Getenv("HIGGSGO_ADMIN_LISTEN"); v != "" {
		c.Server.AdminListen = v
	}
	if v := os.Getenv("HIGGSGO_ADMIN_BEARER"); v != "" {
		c.Server.AdminBearer = v
	}
	if v := os.Getenv("HIGGSGO_INTERNAL_BEARER"); v != "" {
		c.Server.InternalBearer = v
	}
	if v := os.Getenv("HIGGSGO_STORAGE_SQLITE_PATH"); v != "" {
		c.Storage.SQLite.Path = v
	}
	if v := os.Getenv("HIGGSGO_STORAGE_POSTGRES_DSN"); v != "" {
		c.Storage.Postgres.DSN = v
	}
	if v := os.Getenv("HIGGSGO_LOG_LEVEL"); v != "" {
		c.Observability.LogLevel = v
	}
}

// validate performs cheap sanity checks. Deep validation (e.g. reachability
// of proxy URLs) belongs to the adapter startup path.
func (c *Config) validate() error {
	switch c.Storage.Driver {
	case "sqlite":
		if c.Storage.SQLite.Path == "" {
			return errors.New("storage.sqlite.path is required for driver=sqlite")
		}
	case "postgres":
		if c.Storage.Postgres.DSN == "" {
			return errors.New("storage.postgres.dsn is required for driver=postgres")
		}
	case "":
		return errors.New("storage.driver is required")
	default:
		return fmt.Errorf("storage.driver %q not supported", c.Storage.Driver)
	}
	if c.Server.Listen == "" {
		return errors.New("server.listen is required")
	}
	if !c.Modes.Standalone && !c.Modes.CPAPlugin {
		return errors.New("at least one of modes.standalone / modes.cpa_plugin must be enabled")
	}
	return nil
}
