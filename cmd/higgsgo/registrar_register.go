//go:build register
// +build register

package main

// buildRegistrar wires the `-tags register` full-featured variant:
// spawns the plugins/register/driver-node subprocess (Node bridge
// to the higgsfield-register project's registerAccount()) and hands
// it to the plugins/register bridge via Deps.Driver.
//
// Why Driver (not Browser+Mailbox+Captcha) at this wiring step:
// ROADMAP §5.4 P4-3b chose the "one-shot subprocess call per
// registration" model. Node owns the whole flow (Playwright, camoufox,
// DataDome, Graph OTP); Go just enqueues a request and reads the
// harvested result. Browser/Mailbox/Captcha stay reserved for a
// hypothetical future in-Go flow — the field is nil here.
//
// Failure to spawn the Node driver is NOT fatal. The registrar still
// answers Enqueue/List/Get/Retry over the SQLite store; the background
// worker just doesn't process rows. Operators see the "register worker
// skipped" warning and can fix the deployment (missing node, missing
// higgsfield-register sibling, wrong port) without downtime on the
// admin surface. This mirrors the way missing browser+mailbox
// gracefully degrades in the legacy path.

import (
	"context"
	"log/slog"
	"os"
	"strconv"

	"github.com/greensheep999/higgsgo/internal/adapters/registrar/higgsfield"
	"github.com/greensheep999/higgsgo/internal/config"
	"github.com/greensheep999/higgsgo/internal/ports"
	register "github.com/greensheep999/higgsgo/plugins/register"
	"github.com/greensheep999/higgsgo/plugins/register/adapters/camoufox"
)

func buildRegistrar(
	ctx context.Context,
	logger *slog.Logger,
	_ *config.Config,
	regStore ports.RegistrationStore,
	accountStore ports.AccountStore,
) (ports.Registrar, error) {
	logger.Info("registrar: full build (-tags register) — plugins/register bridge active")

	// Try to spawn the Node driver. Read config via env vars for now
	// so operators can experiment without a schema change; a proper
	// config.Registrar section can consolidate these later.
	//   HIGGSGO_REGISTER_DRIVER_URL     — connect to an already-running driver
	//   HIGGSGO_REGISTER_DRIVER_PORT    — port for a locally-spawned driver
	//   HIGGSGO_REGISTER_MAILBOX_CLIENT — Microsoft Graph client_id
	//   HIGGSGO_REGISTER_MAILBOX_TOKEN  — Microsoft Graph refresh_token
	//   HIGGSGO_REGISTER_HEADED         — set to "1" for a visible browser
	opts := camoufox.NodeDriverOptions{
		DriverURL: os.Getenv("HIGGSGO_REGISTER_DRIVER_URL"),
		NodeBin:   os.Getenv("HIGGSGO_NODE_BIN"),
		Headless:  os.Getenv("HIGGSGO_REGISTER_HEADED") != "1",
		Logger:    logger,
		MailboxConfig: camoufox.MailboxConfig{
			ClientID:     os.Getenv("HIGGSGO_REGISTER_MAILBOX_CLIENT"),
			RefreshToken: os.Getenv("HIGGSGO_REGISTER_MAILBOX_TOKEN"),
		},
	}
	if raw := os.Getenv("HIGGSGO_REGISTER_DRIVER_PORT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			opts.Port = n
		}
	}
	var driver register.Driver
	if nd, err := camoufox.New(ctx, opts); err != nil {
		// Non-fatal: log and let the registrar boot without a
		// worker. Admin surface stays live.
		logger.Warn("registrar: node driver spawn failed; worker disabled",
			slog.String("err", err.Error()))
	} else {
		driver = nd
	}

	deps := higgsfield.Deps{
		Store:       regStore,
		Accounts:    accountStore, // ROADMAP §5.4 P4-3c: MarkCompleted upserts here
		Driver:      driver,
		Config:      register.DefaultConfig(),
		Logger:      logger,
		StartWorker: true,
	}
	return higgsfield.NewRegistrar(deps)
}
