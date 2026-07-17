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
	"github.com/greensheep999/higgsgo/internal/core/refresher"
	"github.com/greensheep999/higgsgo/internal/core/regression"
	"github.com/greensheep999/higgsgo/internal/core/webhook"
	"github.com/greensheep999/higgsgo/internal/observability"
	"github.com/greensheep999/higgsgo/internal/ports"
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

	public   *http.Server
	admin    *http.Server
	internal *http.Server
}

// New builds a Server. Handlers are wired up here; concrete route
// registrations (v1 / admin / internal) live in sibling files as they are
// implemented.
func New(cfg *config.Config, logger *slog.Logger, v1Handler *v1.Handler, apiKeys ports.APIKeyStore, accounts ports.AccountStore, jobs ports.JobStore, usage ports.UsageEventStore, groups ports.GroupStore, metrics *observability.Metrics, cpa *cpaplugin.Handler, health ports.ModelHealthStore, webhooks *webhook.Dispatcher, rf *refresher.Refresher, rg *regression.Ticker, audit ports.AuditStore, registry ports.ModelRegistry) *Server {
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
	}

	s.public = &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           s.publicRouter(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.admin = &http.Server{
		Addr:              cfg.Server.AdminListen,
		Handler:           s.adminRouter(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if cfg.Modes.CPAPlugin && cfg.Server.InternalListen != "" {
		s.internal = &http.Server{
			Addr:              cfg.Server.InternalListen,
			Handler:           s.internalRouter(),
			ReadHeaderTimeout: 10 * time.Second,
		}
	}
	return s
}

// ListenAndServe starts all configured listeners. Blocks until ctx is done
// or a listener fails.
func (s *Server) ListenAndServe(ctx context.Context) error {
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
	if s.Metrics != nil {
		// Public /metrics has no auth for now — Prometheus scrapers
		// typically hit an unauthenticated endpoint bound to a private
		// interface. TODO: gate with an IP allowlist or move behind the
		// admin listener once we have a Prom scraper story.
		r.Method(http.MethodGet, "/metrics", s.Metrics.Handler())
	}

	if s.V1 != nil {
		r.Route("/v1", func(r chi.Router) {
			// /v1/models and /v1/models/{alias} are discoverable without
			// authentication so integrators can probe capabilities.
			r.Group(func(r chi.Router) {
				r.Use(middleware.APIKeyAuth(s.APIKeys, true /* optional */))
				r.Get("/models", s.V1.HandleModelsList)
				r.Get("/models/{alias}", s.V1.HandleModelDetail)
			})
			// Everything else requires a valid API key.
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
				r.Post("/videos/generations", s.V1.HandleVideoGeneration)
				r.Post("/images/generations", s.V1.HandleImageGeneration)
				r.Get("/jobs", s.V1.HandleJobsList)
				r.Get("/jobs/{id}", s.V1.HandleJobFetch)
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

	// All admin handlers live under /admin so the URL scheme matches the
	// documented API surface (docs/API_REFERENCE.md, docs/OPERATIONS.md)
	// and the WebUI client which prefixes every request with /admin.
	r.Route("/admin", func(r chi.Router) {
		r.Use(middleware.BearerAuth(s.Config.Server.AdminBearer))
		if s.Audit != nil {
			r.Use(middleware.Audit(s.Audit, s.Logger))
		}
		if s.APIKeys != nil {
			admin.NewKeysHandler(s.APIKeys).Register(r)
		}
		if s.Accounts != nil {
			admin.NewAccountsHandler(s.Accounts).Register(r)
			admin.NewStatsHandler(s.Accounts, s.Jobs).Register(r)
		}
		if s.Jobs != nil {
			admin.NewJobsHandler(s.Jobs).Register(r)
		}
		if s.Usage != nil {
			admin.NewUsageHandler(s.Usage).Register(r)
		}
		if s.Groups != nil {
			admin.NewGroupsHandler(s.Groups).Register(r)
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
	})
	return r
}

func (s *Server) internalRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.Recoverer)
	r.Use(middleware.AccessLog(s.Logger))

	r.Get("/health", s.healthHandler)

	r.Group(func(r chi.Router) {
		r.Use(middleware.BearerAuth(s.Config.Server.InternalBearer))
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

// healthHandler answers all /health probes with a minimal JSON body.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}
