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
	"github.com/greensheep999/higgsgo/internal/api/middleware"
	"github.com/greensheep999/higgsgo/internal/api/v1"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Server is the higgsgo HTTP server. It listens on three ports (public,
// admin, internal). The three routers share middleware but expose different
// endpoints.
type Server struct {
	Config   *config.Config
	Logger   *slog.Logger
	V1       *v1.Handler
	APIKeys  ports.APIKeyStore     // required for /v1 auth and /admin/keys
	Accounts ports.AccountStore    // required for /admin/accounts and /admin/stats
	Jobs     ports.JobStore        // optional; used by /admin/stats
	Usage    ports.UsageEventStore // optional; used by /admin/usage
	Groups   ports.GroupStore      // optional; used by /admin/groups

	public   *http.Server
	admin    *http.Server
	internal *http.Server
}

// New builds a Server. Handlers are wired up here; concrete route
// registrations (v1 / admin / internal) live in sibling files as they are
// implemented.
func New(cfg *config.Config, logger *slog.Logger, v1Handler *v1.Handler, apiKeys ports.APIKeyStore, accounts ports.AccountStore, jobs ports.JobStore, usage ports.UsageEventStore, groups ports.GroupStore) *Server {
	s := &Server{
		Config:   cfg,
		Logger:   logger,
		V1:       v1Handler,
		APIKeys:  apiKeys,
		Accounts: accounts,
		Jobs:     jobs,
		Usage:    usage,
		Groups:   groups,
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

	r.Get("/health", s.healthHandler)

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
				r.Post("/videos/generations", s.V1.HandleVideoGeneration)
				r.Post("/images/generations", s.V1.HandleImageGeneration)
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

	r.Get("/health", s.healthHandler)

	r.Group(func(r chi.Router) {
		r.Use(middleware.BearerAuth(s.Config.Server.AdminBearer))
		if s.APIKeys != nil {
			admin.NewKeysHandler(s.APIKeys).Register(r)
		}
		if s.Accounts != nil {
			admin.NewAccountsHandler(s.Accounts).Register(r)
			admin.NewStatsHandler(s.Accounts, s.Jobs).Register(r)
		}
		if s.Usage != nil {
			admin.NewUsageHandler(s.Usage).Register(r)
		}
		if s.Groups != nil {
			admin.NewGroupsHandler(s.Groups).Register(r)
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
		// internal/* routes for CPA plugin registered here in future patches.
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
