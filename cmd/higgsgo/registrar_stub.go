//go:build !register
// +build !register

package main

// buildRegistrar wires the slim / public-release variant: a stub that
// answers 503 registrar_disabled on every admin.Registrar call. No
// puppeteer / captcha / browser code is linked in. See
// docs/PLUGGABLE.md §0 for why the split exists.
//
// The registrationStore parameter is accepted but ignored — the stub
// has no queue to consume from. Keeping the same signature as the
// -tags register variant lets main.go call one function without a
// build-tagged conditional at the call site.

import (
	"context"
	"log/slog"

	"github.com/greensheep999/higgsgo/internal/adapters/registrar/higgsfield"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/ports"
)

func buildRegistrar(
	_ context.Context,
	logger *slog.Logger,
	_ *config.Config,
	_ ports.RegistrationStore,
	_ ports.AccountStore,
) (ports.Registrar, error) {
	logger.Info("registrar: slim build (stub) — set -tags register for the real bridge")
	return higgsfield.NewRegistrar(higgsfield.Deps{})
}
