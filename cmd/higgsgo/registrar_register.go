//go:build register
// +build register

package main

// buildRegistrar wires the `-tags register` full-featured variant:
// the higgsfield.NewRegistrar bridge to plugins/register. Populates
// the Deps struct with:
//   - Store: the SQLite RegistrationStore built in main.go.
//   - Browser / Mailbox / Captcha: nil for now — real adapters land
//     as they're implemented (ROADMAP §5.4 next steps). The bridge
//     tolerates missing browser/mailbox by starting no worker so the
//     admin surface still works for queue inspection.
//   - Config: register.DefaultConfig() (2 concurrent, 5s poll, 120s
//     OTP timeout, retry limit 3).
//   - Logger: main.go's slog handle.
//   - StartWorker: true — main.go calls .Start(ctx) on the returned
//     registrar so the background poller runs.
//
// See docs/PLUGGABLE.md §0 and internal/adapters/registrar/higgsfield/
// higgsfield.go for the bridge implementation.

import (
	"context"
	"log/slog"

	"github.com/greensheep999/higgsgo/internal/adapters/registrar/higgsfield"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/ports"
	register "github.com/greensheep999/higgsgo/plugins/register"
)

func buildRegistrar(
	_ context.Context,
	logger *slog.Logger,
	_ *config.Config,
	regStore ports.RegistrationStore,
) (ports.Registrar, error) {
	logger.Info("registrar: full build (-tags register) — plugins/register bridge active")

	// Browser / Mailbox / Captcha are intentionally nil at this
	// wiring step. plugins/register/adapters/{camoufox,cloak} are
	// still placeholders (see docs/ROADMAP.md §5.3). Filling one in
	// is the next task; until then the bridge starts the admin
	// surface only, no worker.
	deps := higgsfield.Deps{
		Store:       regStore,
		Browser:     nil,
		Mailbox:     nil,
		Captcha:     nil,
		Config:      register.DefaultConfig(),
		Logger:      logger,
		StartWorker: true,
	}
	return higgsfield.NewRegistrar(deps)
}
