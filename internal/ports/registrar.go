package ports

import (
	"context"
	"time"
)

// Registrar handles the end-to-end higgsfield account registration
// flow: OTP email verification, puppeteer-driven signup, captcha
// solving, workspace creation.
//
// Two implementations live in-tree:
//
//   - A stub (default build). Every method returns
//     domain.ErrRegistrarDisabled so slim / proxy-only deployments
//     ship no puppeteer / captcha code and admin handlers can answer
//     503 with a stable shape instead of an opaque 500.
//   - A real skeleton at
//     internal/adapters/registrar/higgsfield/, compiled only when the
//     binary is built with `-tags register`. Under that tag it will
//     drive puppeteer + a mailbox Provider + a captcha Provider, or
//     alternatively delegate to an external higgsfield-register HTTP
//     service. The signatures below are the contract both paths honour.
//
// Wiring: cmd/higgsgo/main.go calls
// `higgsfield.NewRegistrar(...)` unconditionally; the build tag picks
// which package variant supplies the constructor. `internal/api/server.go`
// then hands the resulting `Registrar` to both the admin listener
// (/admin/registrations/*) and — when Mode B is enabled — the internal
// listener (via the cpaplugin.Handler.Registrar field, wiring TBD).
// Sharing one instance means the CPA partner's async status polls and
// the admin console's list view read from the same store.
type Registrar interface {
	// Enqueue queues a new registration attempt. Returns the assigned
	// registration id so callers can poll GetStatus. Returns
	// domain.ErrRegistrarDisabled when the register build tag was
	// excluded at compile time.
	Enqueue(ctx context.Context, req RegistrationRequest) (id string, err error)

	// GetStatus reads a queued / in-progress / completed registration
	// by id. Returns domain.ErrRegistrationNotFound when the id is
	// unknown, or domain.ErrRegistrarDisabled on the stub.
	GetStatus(ctx context.Context, id string) (*RegistrationRow, error)

	// List returns recent registrations, newest first. `filter.Limit`
	// is bounded by the adapter (max 200); empty filter returns the
	// most recent 50 rows. Status filter matches on
	// RegistrationRow.Status when non-empty.
	List(ctx context.Context, filter RegistrationFilter) ([]RegistrationRow, error)

	// Retry re-queues a failed / stuck registration. No-op on success
	// rows (adapters may return nil or a "cannot retry succeeded row"
	// error — callers should treat both as terminal). Returns
	// domain.ErrRegistrationNotFound when id is unknown.
	Retry(ctx context.Context, id string) error
}

// RegistrationRequest is the input for Registrar.Enqueue.
//
// A blank OAuthSource selects the password flow (mailbox OTP +
// captcha). A non-blank value ("google", "github", …) selects the
// OAuth flow, where the mailbox Provider is not exercised. ProxyURL
// is a per-registration proxy override; when empty, the adapter
// picks a proxy from the operator-configured pool.
type RegistrationRequest struct {
	Email       string
	OAuthSource string
	ProxyURL    string
}

// RegistrationFilter is the input for Registrar.List.
type RegistrationFilter struct {
	Status string
	Since  time.Time
	Limit  int
	Offset int
}

// RegistrationRow is the read model returned by GetStatus / List.
// Status transitions: pending -> running -> (success | failed).
// AccountID is populated only on success; LastError only on failed.
type RegistrationRow struct {
	ID          string
	Email       string
	OAuthSource string
	ProxyURL    string
	Status      string
	Attempts    int
	LastError   string
	AccountID   string
	CreatedAt   time.Time
	FinishedAt  time.Time
}
