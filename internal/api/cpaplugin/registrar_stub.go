package cpaplugin

import (
	"context"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// StubRegistrar is the default (proxy-only) Registrar. Every method
// returns domain.ErrRegistrarDisabled so admin handlers can answer 503
// with a stable shape and the WebUI's Plugin > Registrations tab can
// display a clear "build with `-tags register`" hint instead of an
// opaque 500.
//
// Rationale for the stub living in-tree next to the CPA plugin
// handlers rather than in a standalone adapter package: cpaplugin is
// already the "plugin family" surface (JWT refresh, /internal/register,
// async registration status). Keeping the disabled-state
// implementation adjacent to the CPA handlers keeps the "plugin"
// grouping conceptually intact — the real skeleton with the puppeteer
// / mailbox / captcha dependency chain lives in
// internal/adapters/registrar/higgsfield/ (behind build tag
// "register") where isolation actually matters.
type StubRegistrar struct{}

// Compile-time assertion.
var _ ports.Registrar = (*StubRegistrar)(nil)

// Enqueue always returns domain.ErrRegistrarDisabled.
func (StubRegistrar) Enqueue(ctx context.Context, req ports.RegistrationRequest) (string, error) {
	return "", domain.ErrRegistrarDisabled
}

// GetStatus always returns domain.ErrRegistrarDisabled.
func (StubRegistrar) GetStatus(ctx context.Context, id string) (*ports.RegistrationRow, error) {
	return nil, domain.ErrRegistrarDisabled
}

// List always returns domain.ErrRegistrarDisabled.
func (StubRegistrar) List(ctx context.Context, filter ports.RegistrationFilter) ([]ports.RegistrationRow, error) {
	return nil, domain.ErrRegistrarDisabled
}

// Retry always returns domain.ErrRegistrarDisabled.
func (StubRegistrar) Retry(ctx context.Context, id string) error {
	return domain.ErrRegistrarDisabled
}
