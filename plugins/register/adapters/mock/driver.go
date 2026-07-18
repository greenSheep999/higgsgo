// Package mock provides an in-process Driver used to prove the
// plugins/register plumbing works end-to-end without spinning up a
// browser or a Node subprocess. It's the fastest path from "queue a
// registration" to "see MarkCompleted fire" — and it's what backs the
// integration test that verifies the higgsgo wiring picks up a
// pending row, hands it to the driver, and writes back a success
// account_id.
//
// Not for production: every Register call returns the same synthetic
// account. See ROADMAP §5.4 P4-3b for the real Node subprocess driver
// that replaces this in operator builds.
package mock

import (
	"context"
	"fmt"
	"time"

	register "github.com/greensheep999/higgsgo/plugins/register"
)

// Driver is the fake ports.Driver used by tests. Every field is
// tunable so a test can force success/failure/latency without a
// separate constructor.
type Driver struct {
	// AccountIDPrefix seeds the synthetic account id ("mock_" +
	// email-derived suffix by default). Overridable so tests can
	// distinguish rows produced by two Driver instances.
	AccountIDPrefix string

	// FailWith, when non-nil, makes every Register() return this
	// error instead of a success. Simulates a browser / OTP failure.
	FailWith error

	// Delay, when > 0, sleeps before returning. Simulates a real
	// registration's wall-clock cost — useful for tests that check
	// timeout / concurrency behaviour.
	Delay time.Duration
}

// New returns a Driver with the default account id prefix.
func New() *Driver {
	return &Driver{AccountIDPrefix: "mock"}
}

// Name identifies the driver in logs / metrics.
func (d *Driver) Name() string { return "mock" }

// Register implements register.Driver. Blocks for Delay then returns
// FailWith (if set) or a synthetic CompletedResult.
func (d *Driver) Register(ctx context.Context, req register.RegisterRequest) (register.CompletedResult, error) {
	if d.Delay > 0 {
		select {
		case <-time.After(d.Delay):
		case <-ctx.Done():
			return register.CompletedResult{}, ctx.Err()
		}
	}
	if d.FailWith != nil {
		return register.CompletedResult{}, d.FailWith
	}
	prefix := d.AccountIDPrefix
	if prefix == "" {
		prefix = "mock"
	}
	return register.CompletedResult{
		AccountID: fmt.Sprintf("%s_%s", prefix, sanitize(req.Email)),
		UserID:    fmt.Sprintf("user_%s", sanitize(req.Email)),
		SessionID: "mock_session_token",
		UserAgent: "Mock/1.0 (fake)",
		Cookies: []register.Cookie{
			{Name: "__session", Value: "mock_session_token", Domain: "higgsfield.ai"},
		},
		DataDomeID: "mock_datadome_id",
		PlanType:   "starter",
		Credits:    100,
	}, nil
}

// sanitize turns an email into a safe suffix for the synthetic
// account id (strip @, replace punctuation with _). Not RFC-strict —
// only produces a token suitable for a mock id.
func sanitize(email string) string {
	out := make([]byte, 0, len(email))
	for i := 0; i < len(email); i++ {
		c := email[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
