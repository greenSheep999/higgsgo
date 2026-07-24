// Package costsync is a background ticker that keeps the model registry's
// per-model cost estimates in sync with higgsfield's live pricing. Every
// Interval it picks one active account, calls GET /job-sets/costs, and
// pushes the resulting JST → credit-hundredths map into the registry via
// CostSink.SetDynamicCosts — replacing the hand-copied static cost table
// baked into verified-models.json.
//
// The endpoint is account-agnostic (pricing is global, and the route even
// works unauthenticated), so unlike the claimer / invoicewatch tickers we
// do NOT fan out over the whole pool: one successful fetch refreshes the
// prices for every model at once. We just need any one healthy account to
// carry the standard headers so DataDome doesn't challenge us.
//
// Read-only against higgsfield and against the accounts store; failures are
// logged at warn and never fed to the failover controller (a stale price
// table is not an account-health signal). Mirrors failover.Recoverer /
// claimer.Claimer: one struct, one ticker, one goroutine launched by
// main.go.
package costsync

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"time"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
	"github.com/greensheep999/higgsgo/internal/util/idgen"
)

// DefaultInterval is the refresh cadence. 6h is frequent enough that a
// price change propagates the same day, and light enough that the single
// upstream GET is negligible.
const DefaultInterval = 6 * time.Hour

// CostSink is the narrow slice of the model registry costsync writes to.
// Defined locally so this package doesn't depend on the concrete
// jsonstatic.Registry — any registry that accepts a JST → credit-hundredths
// overlay satisfies it. Implemented by *jsonstatic.Registry.
type CostSink interface {
	SetDynamicCosts(costs map[string]int64)
}

// Syncer refreshes the registry's dynamic cost overlay on a fixed cadence.
type Syncer struct {
	Accounts ports.AccountStore
	Upstream *upstream.Client
	Registry CostSink
	Pricing  ports.PricingStore
	Logger   *slog.Logger
	Interval time.Duration // zero selects DefaultInterval
}

// New builds a Syncer with the default interval filled. Interval can be
// overridden by the caller after construction.
func New(accounts ports.AccountStore, up *upstream.Client, registry CostSink, logger *slog.Logger) *Syncer {
	return &Syncer{
		Accounts: accounts,
		Upstream: up,
		Registry: registry,
		Logger:   logger,
		Interval: DefaultInterval,
	}
}

// Run blocks until ctx is canceled. Intended as `go s.Run(ctx)`. A nil
// receiver or unwired dependency returns immediately so callers need not
// guard the launch.
func (s *Syncer) Run(ctx context.Context) {
	if s == nil || s.Accounts == nil || s.Upstream == nil || s.Registry == nil {
		return
	}
	if s.Interval <= 0 {
		s.Interval = DefaultInterval
	}
	if s.Logger != nil {
		s.Logger.Info("costsync starting", slog.Duration("interval", s.Interval))
	}
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	s.once(ctx) // immediate first pass so boot serves live prices ASAP
	for {
		select {
		case <-ctx.Done():
			if s.Logger != nil {
				s.Logger.Info("costsync stopping")
			}
			return
		case <-ticker.C:
			s.once(ctx)
		}
	}
}

// TriggerOnce runs a single sync pass synchronously. Used by tests and
// (optionally) an admin trigger endpoint.
func (s *Syncer) TriggerOnce(ctx context.Context) {
	s.once(ctx)
}

// once performs one refresh: pick the first active account, fetch the live
// cost table, push it into the registry. Any failure short-circuits with a
// warn and leaves the previous overlay in place (stale prices beat none).
func (s *Syncer) once(ctx context.Context) {
	if s == nil || s.Accounts == nil || s.Upstream == nil || s.Registry == nil {
		return
	}
	accounts, err := s.Accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("costsync list accounts", slog.String("err", err.Error()))
		}
		return
	}
	if len(accounts) == 0 {
		if s.Logger != nil {
			s.Logger.Warn("costsync: no active account to carry cost fetch")
		}
		return
	}
	acc := accounts[0]
	catalog, err := s.Upstream.FetchJobSetCostCatalog(ctx, &acc)
	if err != nil {
		if s.Logger != nil {
			s.Logger.Warn("costsync fetch job-set costs",
				slog.String("account_id", acc.ID),
				slog.String("err", err.Error()))
		}
		return
	}
	if catalog == nil || len(catalog.MinCosts) == 0 {
		if s.Logger != nil {
			s.Logger.Warn("costsync: empty cost table, keeping previous overlay")
		}
		return
	}
	if s.Pricing != nil {
		now := time.Now().UTC()
		hash := sha256.Sum256([]byte(catalog.RawJSON))
		snapshot := &domain.PricingSnapshot{
			ID:            idgen.NewID("price"),
			Source:        "higgs_job_set_costs",
			SourceURL:     "/job-sets/costs",
			PayloadJSON:   catalog.RawJSON,
			PayloadSHA256: fmt.Sprintf("%x", hash[:]),
			FetchedAt:     now,
		}
		for i := range catalog.Rules {
			catalog.Rules[i].ID = idgen.NewID("price_rule")
			catalog.Rules[i].SnapshotID = snapshot.ID
			catalog.Rules[i].ObservedAt = now
		}
		if err := s.Pricing.SaveSnapshot(ctx, snapshot, catalog.Rules); err != nil {
			if s.Logger != nil {
				s.Logger.Warn("costsync persist pricing snapshot",
					slog.String("snapshot_id", snapshot.ID),
					slog.String("err", err.Error()))
			}
			return
		}
	}
	s.Registry.SetDynamicCosts(catalog.MinCosts)
	if s.Logger != nil {
		s.Logger.Info("costsync refreshed dynamic costs",
			slog.Int("models", len(catalog.MinCosts)),
			slog.Int("rules", len(catalog.Rules)))
	}
}
