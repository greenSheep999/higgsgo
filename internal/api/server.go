// Package api hosts HTTP handlers for the /v1, /admin, and /internal surfaces.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/greensheep999/higgsgo/internal/api/admin"
	"github.com/greensheep999/higgsgo/internal/api/cpaplugin"
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/api/v1"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/core/bearer"
	"github.com/greensheep999/higgsgo/internal/core/refresher"
	"github.com/greensheep999/higgsgo/internal/core/regression"
	"github.com/greensheep999/higgsgo/internal/core/webhook"
	"github.com/greensheep999/higgsgo/internal/observability"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/greensheep999/higgsgo/internal/version"
	"github.com/greensheep999/higgsgo/webui"
)

// Server is the higgsgo HTTP server. It listens on three ports (public,
// admin, internal). The three routers share middleware but expose different
// endpoints.
type Server struct {
	Config     *config.Config
	Logger     *slog.Logger
	V1         *v1.Handler
	APIKeys    ports.APIKeyStore      // required for /v1 auth and /admin/keys
	Accounts   ports.AccountStore     // required for /admin/accounts and /admin/stats
	Jobs       ports.JobStore         // optional; used by /admin/stats
	Usage      ports.UsageEventStore  // optional; used by /admin/usage
	Groups     ports.GroupStore       // optional; used by /admin/groups
	Health     ports.ModelHealthStore // optional; used by /admin/model-health
	Metrics    *observability.Metrics // optional; enables /metrics + per-request instrumentation
	CPAPlugin  *cpaplugin.Handler     // optional; enables /internal/* (Mode B)
	Webhooks   *webhook.Dispatcher    // optional; enables /admin/webhooks/stats
	Refresher  *refresher.Refresher   // optional; enables /admin/tickers/refresher
	Regression *regression.Ticker     // optional; enables /admin/tickers/regression
	Audit      ports.AuditStore       // optional; enables admin write auditing + /admin/audit
	Registry   ports.ModelRegistry    // optional; enables /admin/models/reload
	Registrar  ports.Registrar        // optional; enables /admin/registrations (stub answers 503 disabled)
	Settings   ports.SettingsStore    // optional; enables /admin/settings/* runtime-editable knobs
	// ModelOverrides is optional; enables /admin/models/overrides and
	// the per-alias /admin/models/{alias}/override surface. When
	// wired, the same store is expected to have been handed to
	// Registry.SetOverrideProvider so read paths see the merged view.
	ModelOverrides ports.ModelOverrideStore
	// Failover wiring. Any nil pointer here disables the corresponding
	// /admin/failover/* routes; the accounts / events / overrides
	// stores together are what makes the surface useful.
	FailoverEvents    ports.FailoverEventStore
	FailoverOverrides ports.FailoverOverridesStore
	FailoverConfig    *config.FailoverConfig
	// Bearer is the runtime-mutable admin bearer manager. Required for
	// /admin/*: BearerAuth reads Current() on every request so a
	// rotation via POST /admin/settings/bearer/rotate takes effect
	// without a restart. Nil disables the whole /admin surface (New
	// falls back to StaticBearer over cfg.Server.AdminBearer so the
	// server never boots wide-open even if the caller forgot to wire
	// this in).
	Bearer *bearer.Manager

	// Prober is optional; when set, powers POST
	// /admin/accounts/{id}/probe by actively pinging the account
	// through the upstream client. main.go builds a thin adapter
	// around *upstream.Client so this file stays free of a hard
	// upstream dependency (keeps the layering discipline documented
	// in PLUGGABLE §8).
	Prober admin.Prober

	public   *http.Server
	admin    *http.Server
	internal *http.Server
}

// New builds a Server. Handlers are wired up here; concrete route
// registrations (v1 / admin / internal) live in sibling files as they are
// implemented.
func New(cfg *config.Config, logger *slog.Logger, v1Handler *v1.Handler, apiKeys ports.APIKeyStore, accounts ports.AccountStore, jobs ports.JobStore, usage ports.UsageEventStore, groups ports.GroupStore, metrics *observability.Metrics, cpa *cpaplugin.Handler, health ports.ModelHealthStore, webhooks *webhook.Dispatcher, rf *refresher.Refresher, rg *regression.Ticker, audit ports.AuditStore, registry ports.ModelRegistry, settings ports.SettingsStore, bearerMgr *bearer.Manager) *Server {
	s := &Server{
		Config:     cfg,
		Logger:     logger,
		V1:         v1Handler,
		APIKeys:    apiKeys,
		Accounts:   accounts,
		Jobs:       jobs,
		Usage:      usage,
		Groups:     groups,
		Health:     health,
		Metrics:    metrics,
		CPAPlugin:  cpa,
		Webhooks:   webhooks,
		Refresher:  rf,
		Regression: rg,
		Audit:      audit,
		Registry:   registry,
		Settings:   settings,
		Bearer:     bearerMgr,
	}

	// Build stub http.Server structs; routers are attached at
	// ListenAndServe time so late-assigned dependencies (e.g.
	// FailoverEvents / ModelOverrides set after New() returns)
	// are visible when routes register.
	s.public = &http.Server{
		Addr:              cfg.Server.Listen,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.admin = &http.Server{
		Addr:              cfg.Server.AdminListen,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.Modes.CPAPlugin && cfg.Server.InternalListen != "" {
		s.internal = &http.Server{
			Addr:              cfg.Server.InternalListen,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	return s
}

// ListenAndServe starts all configured listeners. Blocks until ctx is done
// or a listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
	// Attach routers lazily so late field assignments (e.g.
	// FailoverEvents / ModelOverrides) participate in route
	// registration.
	s.public.Handler = s.publicRouter()
	s.admin.Handler = s.adminRouter()
	if s.internal != nil {
		s.internal.Handler = s.internalRouter()
	}
	errCh := make(chan error, 3)
	go func() {
		s.Logger.Info("public listener starting", slog.String("addr", s.public.Addr))
		if err := s.public.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	go func() {
		s.Logger.Info("admin listener starting", slog.String("addr", s.admin.Addr))
		if err := s.admin.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	if s.internal != nil {
		go func() {
			s.Logger.Info("internal listener starting", slog.String("addr", s.internal.Addr))
			if err := s.internal.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				errCh <- err
			}
		}()
	}
	select {
	case <-ctx.Done():
		return s.shutdown()
	case err := <-errCh:
		_ = s.shutdown()
		return err
	}
}

func (s *Server) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.public.Shutdown(ctx)
	_ = s.admin.Shutdown(ctx)
	if s.internal != nil {
		_ = s.internal.Shutdown(ctx)
	}
	return nil
}

func (s *Server) publicRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	r.Use(middleware.AccessLog(s.Logger))
	if s.Metrics != nil {
		r.Use(middleware.HTTPMetrics(s.Metrics, middleware.DefaultRoutePattern))
	}

	r.Get("/health", s.healthHandler)
	// /metrics is intentionally NOT mounted on the public listener. It
	// was moved to the admin listener behind BearerAuth to avoid leaking
	// route cardinality, error rates, and traffic patterns to any
	// internet-facing caller. See adminRouter() and
	// server_metrics_auth_test.go for the new mount + regression tests.

	if s.V1 != nil {
		r.Route("/v1", func(r chi.Router) {
			// /v1/models and /v1/models/{alias} are discoverable without
			// authentication so integrators can probe capabilities.
			r.Group(func(r chi.Router) {
				r.Use(middleware.APIKeyAuth(s.APIKeys, true /* optional */))
				r.Get("/models", s.V1.HandleModelsList)
				r.Get("/models/{alias}", s.V1.HandleModelDetail)
			})
			// Non-playground /v1 traffic requires a real sk-hg- API key.
			r.Group(func(r chi.Router) {
				if s.APIKeys != nil {
					r.Use(middleware.APIKeyAuth(s.APIKeys, false))
				}
				// Per-API-key token bucket. Sits AFTER auth so it can key
				// off the resolved APIKey.ID in ctx; unauthenticated
				// requests would already have been rejected above.
				rl := &middleware.RateLimit{
					RPS:    s.Config.Server.RateLimit.RPS,
					Burst:  s.Config.Server.RateLimit.Burst,
					Logger: s.Logger,
				}
				r.Use(rl.Middleware)
				// Video generation is reachable on two paths that share a
				// handler:
				//   * /videos/generations — original higgsgo path (kept as
				//     legacy alias for any client already integrated).
				//   * /video/generations  — new-api / OneAPI compatibility
				//     shape, singular "video". Preferred going forward.
				// OpenAI's own canonical endpoint is POST /v1/videos (no
				// /generations suffix, different body shape); we don't
				// mount that yet — no caller has asked for it and its body
				// contract diverges from images/generations.
				r.Post("/videos/generations", s.V1.HandleVideoGeneration)
				r.Post("/video/generations", s.V1.HandleVideoGeneration)
				r.Post("/images/generations", s.V1.HandleImageGeneration)
				r.Get("/jobs", s.V1.HandleJobsList)
				r.Get("/jobs/{id}", s.V1.HandleJobFetch)
			})
			// Playground surface for the WebUI. Uses a bespoke auth
			// middleware that accepts either the deploy-wide admin bearer
			// (treated as scope=full so the console can drive every model
			// without minting a key) or a sk-hg- API key (subject to the
			// column-level PlaygroundScope check enforced by the gate).
			// Mounted as its own group — outside the APIKeyAuth block —
			// so admin bearer traffic is not rejected before reaching the
			// scope resolver in the handler.
			r.Route("/playground", func(r chi.Router) {
				r.Use(middleware.PlaygroundAuth(s.adminBearerAccepter(), s.APIKeys))
				// Rate-limit still applies to API-key callers (bucket keyed
				// off APIKey.ID). Admin bearer callers land with no APIKey
				// in ctx and pass through unlimited — acceptable given the
				// low-volume console traffic pattern.
				rl := &middleware.RateLimit{
					RPS:    s.Config.Server.RateLimit.RPS,
					Burst:  s.Config.Server.RateLimit.Burst,
					Logger: s.Logger,
				}
				r.Use(rl.Middleware)
				r.Use(middleware.PlaygroundGate())
				r.Get("/models", s.V1.HandlePlaygroundModels)
				r.Post("/estimate", s.V1.HandlePlaygroundEstimate)
				r.Post("/execute", s.V1.HandlePlaygroundExecute)
			})
		})
	}
	return r
}

func (s *Server) adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(middleware.AccessLog(s.Logger))
	if s.Metrics != nil {
		r.Use(middleware.HTTPMetrics(s.Metrics, middleware.DefaultRoutePattern))
	}
	// CORS sits at the outermost auth boundary so preflight OPTIONS
	// requests short-circuit before BearerAuth would 401 them. Only
	// attached when the operator supplied an allowlist; otherwise the
	// admin surface stays same-origin.
	if len(s.Config.Server.WebUI.Origins) > 0 {
		corsMW := &middleware.CORS{AllowedOrigins: s.Config.Server.WebUI.Origins}
		r.Use(corsMW.Middleware)
	}

	r.Get("/health", s.healthHandler)

	// /metrics lives on the admin listener behind BearerAuth. Prom
	// scrapers reach it with the same admin bearer every other admin
	// route uses; the public listener has no /metrics route at all.
	// Nil-safe: when Server.Metrics is unset the whole route is skipped
	// and the SPA fallback below claims the path — see
	// server_metrics_auth_test.go for the regression coverage.
	if s.Metrics != nil {
		r.Group(func(r chi.Router) {
			r.Use(middleware.BearerAuth(s.adminBearerAccepter()))
			r.Method(http.MethodGet, "/metrics", s.Metrics.Handler())
		})
	}

	// Mirror the playground surface on the admin listener so the WebUI
	// can drive it through a single base URL (VITE_HIGGSGO_ADMIN_URL).
	// The public listener still hosts the canonical /v1/playground/*
	// with API-key auth; this mirror only accepts the admin bearer
	// (treated as scope=full) and reuses the same handlers. Rate limit
	// and PlaygroundGate still apply — admin bearer bypasses the gate
	// scope check downstream via IsAdminBearer marker.
	if s.V1 != nil {
		r.Route("/v1/playground", func(r chi.Router) {
			r.Use(middleware.PlaygroundAuth(s.adminBearerAccepter(), s.APIKeys))
			rl := &middleware.RateLimit{
				RPS:    s.Config.Server.RateLimit.RPS,
				Burst:  s.Config.Server.RateLimit.Burst,
				Logger: s.Logger,
			}
			r.Use(rl.Middleware)
			r.Use(middleware.PlaygroundGate())
			r.Get("/models", s.V1.HandlePlaygroundModels)
			r.Post("/estimate", s.V1.HandlePlaygroundEstimate)
			r.Post("/execute", s.V1.HandlePlaygroundExecute)
		})

		// Mirror the read-only /v1/models catalog on the admin listener
		// too. The WebUI needs it for the group model picker; it already
		// talks to a single admin base URL with the admin bearer, so
		// re-issuing the public listener's mount here (behind admin
		// bearer) keeps the "one base URL" invariant.
		r.Group(func(r chi.Router) {
			r.Use(middleware.BearerAuth(s.adminBearerAccepter()))
			r.Get("/v1/models", s.V1.HandleModelsList)
			r.Get("/v1/models/{alias}", s.V1.HandleModelDetail)
		})
	}

	// All admin handlers live under /admin so the URL scheme matches the
	// documented API surface (docs/API_REFERENCE.md, docs/OPERATIONS.md)
	// and the WebUI client which prefixes every request with /admin.
	r.Route("/admin", func(r chi.Router) {
		r.Use(middleware.BearerAuth(s.adminBearerAccepter()))
		if s.Audit != nil {
			r.Use(middleware.Audit(s.Audit, s.Logger))
		}
		if s.APIKeys != nil {
			kh := admin.NewKeysHandler(s.APIKeys)
			// Optional dependencies for the read-only detail endpoints
			// (/keys/{id}/groups and /keys/{id}/stats). Nil-safe on the
			// handler side so the mutating routes still work if either
			// store is not wired in a slimmer deployment.
			kh.Groups = s.Groups
			kh.Usage = s.Usage
			kh.Register(r)
		}
		if s.Accounts != nil {
			ah := admin.NewAccountsHandler(s.Accounts)
			ah.Registry = s.Registry
			// Prober is optional. When s.Prober is nil the handler's
			// /accounts/{id}/probe answers 503 probe_disabled instead
			// of pretending to work.
			if s.Prober != nil {
				ah.Prober = s.Prober
			}
			ah.Register(r)
			admin.NewStatsHandler(s.Accounts, s.Jobs).Register(r)
		}
		if s.Jobs != nil {
			admin.NewJobsHandler(s.Jobs).Register(r)
		}
		if s.Usage != nil {
			admin.NewUsageHandler(s.Usage).Register(r)
		}
		if s.Groups != nil {
			gh := admin.NewGroupsHandler(s.Groups)
			// Wire the SettingsStore so POST /admin/groups can fall
			// back to the operator-configured routing_strategy_default
			// when no explicit route_strategy is supplied.
			gh.Settings = s.Settings
			gh.Register(r)
		}
		if s.Health != nil {
			admin.NewModelHealthHandler(s.Health).Register(r)
		}
		if s.Webhooks != nil {
			admin.NewWebhooksHandler(s.Webhooks).Register(r)
		}
		admin.NewTickersHandler(s.Refresher, s.Regression, s.Logger).Register(r)
		if s.Audit != nil {
			admin.NewAuditHandler(s.Audit).Register(r)
		}
		if s.Registry != nil {
			admin.NewModelsHandler(s.Registry, s.Logger).Register(r)
		}
		// Settings surface: currently only /admin/settings/bearer,
		// wired only when both a persistence store and a bearer.Manager
		// were supplied. Slimmer deployments that skip settings still
		// boot; the WebUI's rotate dialog will simply 404.
		if s.Settings != nil && s.Bearer != nil {
			admin.NewSettingsHandler(s.Bearer, s.Settings).Register(r)
		}
		// Routing-strategy default (nil-safe: falls back to
		// round_robin on Create when Settings is missing).
		admin.NewRoutingSettingsHandler(s.Settings).Register(r)
		// Advanced load_balance knobs (tier-aware, jitter, headroom, …).
		// Mounted only when a SettingsStore is wired — without one the
		// PUT would have nowhere to persist. Reads on the pool side
		// remain safe because ResolveLoadBalanceSettings is nil-safe
		// and returns the hardcoded defaults.
		if s.Settings != nil {
			admin.NewLoadBalanceSettingsHandler(s.Settings).Register(r)
		}
		// Model overrides layered on top of the static jsonstatic
		// catalog. Requires a Registry so writes can trigger a Reload
		// and downstream reads see the merged view immediately.
		if s.ModelOverrides != nil && s.Registry != nil {
			admin.NewModelOverridesHandler(s.ModelOverrides, s.Registry, s.Logger).Register(r)
		}
		// Failover / auto-isolation admin surface. Wired only when all
		// three dependencies (accounts + events + config) are present.
		if s.FailoverConfig != nil && s.FailoverEvents != nil && s.FailoverOverrides != nil {
			admin.NewFailoverHandler(s.Accounts, s.FailoverEvents, s.FailoverOverrides, s.FailoverConfig, s.Logger).Register(r)
		}
		// Version + update-check surface. Always mounted — the current
		// version is useful even when Updates.CheckEnabled=false, since
		// operators can still confirm which build is running.
		admin.NewVersionHandler(
			s.Config.Updates.GitHubOwner,
			s.Config.Updates.GitHubRepo,
			s.Config.Updates.CheckEnabled,
		).Register(r)
		// Registrations (plugin family): always mounted so the WebUI's
		// Registrations tab renders a stable "registrar_disabled" 503
		// on slim builds instead of a 404. When the binary is built
		// with -tags register, main.go supplies a real Registrar and
		// these routes come alive.
		admin.NewRegistrationsHandler(s.Registrar).Register(r)
	})

	// SPA fallback. Serves the embedded webui/ dist for any path that
	// wasn't matched above. Bearer auth is enforced by the /admin/*
	// XHR calls the SPA makes at runtime; the static assets themselves
	// (HTML/JS/CSS) are non-sensitive and don't need a token to fetch.
	spa := webui.Handler()
	r.NotFound(spa.ServeHTTP)
	r.Get("/", spa.ServeHTTP)
	return r
}

func (s *Server) internalRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(middleware.AccessLog(s.Logger))

	r.Get("/health", s.healthHandler)

	r.Group(func(r chi.Router) {
		r.Use(middleware.BearerAuth(middleware.StaticBearer(s.Config.Server.InternalBearer)))
		// The /internal/* surface performs writes (register, execute,
		// delete, refresh_jwt) driven by the upstream CPA platform.
		// Audit those the same way we audit /admin/* so operators have
		// a single write history to correlate against usage_events.
		if s.Audit != nil {
			r.Use(middleware.Audit(s.Audit, s.Logger))
		}
		if s.CPAPlugin != nil {
			s.CPAPlugin.Register(r)
		}
	})
	return r
}

// adminBearerAccepter returns the middleware.BearerAccepter used to
// gate /admin/* and the /v1/playground/* mirror on the admin listener.
// The runtime-mutable bearer.Manager wins when wired; otherwise we
// fall back to a StaticBearer over the boot-time TOML value so the
// server never accidentally boots wide-open.
func (s *Server) adminBearerAccepter() middleware.BearerAccepter {
	if s.Bearer != nil {
		return s.Bearer
	}
	return middleware.StaticBearer(s.Config.Server.AdminBearer)
}

// healthHandler answers all /health probes with a minimal JSON body. The
// version field is included so uptime probes / load balancers can confirm
// the exact build behind each replica without hitting /admin/version.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"time":    time.Now().UTC().Format(time.RFC3339),
		"version": version.Info().Version,
	})
}
