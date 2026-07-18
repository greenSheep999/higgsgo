// Package higgsfield is the real ports.Registrar implementation for
// the higgsfield.ai signup flow. It is wired into cmd/higgsgo/main.go
// only when the binary is built with `-tags register`; without the
// tag the sibling higgsfield_disabled.go returns a cpaplugin.StubRegistrar
// so the /admin/registrations surface answers 503 with a stable shape
// and the SPA's Plugin > Registrations tab shows a clear opt-in hint
// instead of failing opaquely.
//
// Both files export `NewRegistrar(deps Deps) ports.Registrar`, so
// main.go can import this package unconditionally and let the build
// tag pick the variant. `Deps` is defined in both files — empty in
// the disabled path, populated with Mailbox / Captcha / Browser
// fields in the real path — so the call site (`higgsfield.Deps{}`
// in cmd/higgsgo/main.go) compiles either way.
//
// Roadmap (whoever picks this up next):
//
//   - Option A — port the flow: translate the higgsfield-register
//     Node/puppeteer project into Go. This means adding
//     Mailbox / Captcha / Browser Provider ports (they already
//     exist under internal/ports/) as fields on Deps and wiring the
//     real puppeteer driver as a Browser adapter. Migrations 001+
//     already have a `registrations` table aligned with
//     ports.RegistrationRow, so the store side is ready.
//
//   - Option B — HTTP delegator: keep the Node project running as
//     an external service and add an http-call adapter here that
//     POSTs Enqueue and GETs List/Get/Retry against it. Simpler,
//     avoids porting puppeteer, but adds an operational hop.
//
// Both options satisfy the same ports.Registrar interface so callers
// (admin handler + cpaplugin.Handler.Registrar) never see the choice.
package higgsfield
