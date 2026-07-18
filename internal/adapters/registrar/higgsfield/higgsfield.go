//go:build register
// +build register

package higgsfield

import (
	"context"

	"github.com/greensheep999/higgsgo/internal/ports"
)

// Deps is the dependency bag NewRegistrar takes when the register tag
// is set. Fields are TODO — populate with Mailbox / Captcha / Browser
// ports (already defined under internal/ports/) plus a persistence
// hook once the real flow lands. Keeping this a struct (rather than
// positional args) means main.go can add fields without touching
// existing call sites.
type Deps struct {
	// TODO(registrar): Mailbox   ports.Mailbox
	// TODO(registrar): Captcha   ports.CaptchaSolver
	// TODO(registrar): Browser   ports.BrowserAutomator
	// TODO(registrar): Store     ports.RegistrationStore  // future — persist rows
	// TODO(registrar): Proxies   ports.ProxyProvider      // per-registration proxy
	// TODO(registrar): Logger    *slog.Logger
}

// NewRegistrar returns a ports.Registrar backed by the real
// higgsfield.ai signup flow. Under this build tag it is currently a
// panic-TODO skeleton so unrelated code paths (main wiring, admin
// handler, cpaplugin) compile and vet cleanly. Filling in the
// puppeteer + OTP + captcha logic is the follow-up task; the
// signatures below are the stable contract.
func NewRegistrar(deps Deps) ports.Registrar {
	return &registrar{deps: deps}
}

type registrar struct {
	deps Deps
}

// Compile-time assertion so the interface stays in sync.
var _ ports.Registrar = (*registrar)(nil)

func (r *registrar) Enqueue(ctx context.Context, req ports.RegistrationRequest) (string, error) {
	// TODO(registrar): translate higgsfield-register's queue logic
	// (or delegate over HTTP). Must persist the pending row before
	// returning the id so GetStatus can read it back.
	panic("TODO: register implementation — Enqueue")
}

func (r *registrar) GetStatus(ctx context.Context, id string) (*ports.RegistrationRow, error) {
	// TODO(registrar): look up the row by id. Return
	// domain.ErrRegistrationNotFound when unknown.
	panic("TODO: register implementation — GetStatus")
}

func (r *registrar) List(ctx context.Context, filter ports.RegistrationFilter) ([]ports.RegistrationRow, error) {
	// TODO(registrar): read recent rows, newest first, applying
	// status / since / limit / offset from the filter.
	panic("TODO: register implementation — List")
}

func (r *registrar) Retry(ctx context.Context, id string) error {
	// TODO(registrar): re-queue a failed row. No-op on success rows.
	panic("TODO: register implementation — Retry")
}
