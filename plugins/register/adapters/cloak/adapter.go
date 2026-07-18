// Package cloak implements BrowserAutomator via a Node.js Cloak subprocess.
// Communication happens over JSON-RPC on stdin/stdout of the child process.
// This is a placeholder — the subprocess protocol is not yet implemented.
package cloak

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	register "github.com/greensheep999/higgsgo/plugins/register"
)

type Automator struct {
	log *slog.Logger
}

func New(log *slog.Logger) *Automator {
	return &Automator{log: log}
}

func (a *Automator) Name() string { return "cloak" }

func (a *Automator) Launch(_ context.Context, _ register.LaunchOpts) (register.BrowserSession, error) {
	a.log.Warn("cloak: Launch not implemented — subprocess bridge pending")
	return nil, fmt.Errorf("cloak adapter not implemented")
}

// Session is a placeholder that satisfies BrowserSession.
type Session struct{}

func (s *Session) Goto(_ context.Context, _ string) error                         { return fmt.Errorf("not implemented") }
func (s *Session) Fill(_ context.Context, _, _ string) error                      { return fmt.Errorf("not implemented") }
func (s *Session) Click(_ context.Context, _ string) error                        { return fmt.Errorf("not implemented") }
func (s *Session) WaitFor(_ context.Context, _ string, _ time.Duration) error     { return fmt.Errorf("not implemented") }
func (s *Session) Cookies(_ context.Context) ([]register.Cookie, error)           { return nil, fmt.Errorf("not implemented") }
func (s *Session) LocalStorage(_ context.Context, _ string) (string, error)       { return "", fmt.Errorf("not implemented") }
func (s *Session) UserAgent(_ context.Context) (string, error)                    { return "", fmt.Errorf("not implemented") }
func (s *Session) EvalJS(_ context.Context, _ string) (string, error)             { return "", fmt.Errorf("not implemented") }
func (s *Session) Close() error                                                   { return nil }
