//go:build !register
// +build !register

package higgsfield

import (
	"github.com/greensheep999/higgsgo/internal/api/cpaplugin"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// Deps is a zero-field placeholder so the higgsfield.NewRegistrar
// call site in cmd/higgsgo/main.go compiles with or without the
// "register" build tag. When the tag is set, higgsfield.go replaces
// this with a populated dep bag (Mailbox / Captcha / Browser /
// RegistrationStore).
type Deps struct{}

// NewRegistrar returns the stub Registrar that answers 503 on every
// admin call. Slim / proxy-only deploys build with this variant so
// no puppeteer / captcha code is linked in.
//
// Signature matches the -tags register variant's
// `func(Deps) (ports.Registrar, error)` so cmd/higgsgo/main.go
// doesn't need a build-tag switch at the call site. The stub never
// errors — the error return is purely for symmetry with the real
// bridge, which has to validate its dependency bag at construction.
func NewRegistrar(_ Deps) (ports.Registrar, error) {
	return cpaplugin.StubRegistrar{}, nil
}
