// Package bearer holds the runtime-mutable admin bearer manager. The
// bearer used by /admin/* and the WebUI can originate from either the
// TOML config (at boot) or a runtime override (via POST
// /admin/settings/bearer/rotate) — the Manager unifies both sources
// behind an Accepts()/Current() surface so callers do not have to
// care where the current secret came from.
//
// A short overlap window (see GraceWindow) lets the previous bearer
// keep passing auth for a few seconds after Rotate — in-flight
// requests would otherwise 401 the instant the operator hits "save".
// The window uses atomic pointers so BearerAuth reads without locks.
package bearer

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// SettingKey is the system_settings row key used to persist the
// admin bearer override. Exported so admin handlers writing to the
// store share the constant with the manager.
const SettingKey = "admin_bearer"

// GraceWindow is how long a rotated-out bearer keeps being accepted
// after Rotate. Long enough to cover an in-flight XHR waiting on the
// SPA, short enough that a leaked bearer is not usable for minutes.
const GraceWindow = 30 * time.Second

// GeneratedBearerBytes is the entropy source width for a
// server-generated bearer. 32 bytes → 64 hex chars.
const GeneratedBearerBytes = 32

// MinBearerLength enforces a lower bound on operator-supplied
// bearers so a rotate cannot land us with an easily-guessable token.
const MinBearerLength = 16

// ErrEmptyBearer is returned by Rotate when the new bearer is
// empty. Empty bearers would render BearerAuth undefined ("server
// not configured for auth" 500) so we refuse them at the door.
var ErrEmptyBearer = errors.New("bearer cannot be empty")

// ErrBearerTooShort is returned by Rotate when the new bearer is
// shorter than MinBearerLength. The rotate handler surfaces this as
// a 400 error to the client.
var ErrBearerTooShort = fmt.Errorf("bearer must be at least %d characters", MinBearerLength)

// ErrBearerWhitespace is returned by Rotate when the new bearer
// contains whitespace. Whitespace is legal in HTTP header values
// under RFC 7230 but every downstream client strips or mangles it,
// so we refuse it at the door.
var ErrBearerWhitespace = errors.New("bearer must not contain whitespace")

// Source describes where Current() came from — the TOML file that
// booted the process, or a DB-persisted runtime override. Used by
// the /admin/settings/bearer read endpoint to help operators
// distinguish "still on the deploy default" from "someone rotated it".
type Source string

const (
	// SourceTOML means no DB override was found on Load; Current()
	// returns the value passed to New().
	SourceTOML Source = "toml"
	// SourceDB means Load() found a row in system_settings and
	// Current() returns that value.
	SourceDB Source = "db"
)

// Manager owns the current admin bearer and the short-lived grace
// window during a rotation. Zero locks on the hot path: reads use
// atomic.Pointer / atomic.Int64 so BearerAuth stays cheap.
type Manager struct {
	tomlBearer string
	store      ports.SettingsStore
	logger     *slog.Logger

	// current is the currently-accepted bearer. Always non-nil and
	// non-empty after Load.
	current atomic.Pointer[string]

	// previous is the bearer that was current before the last
	// Rotate. Nil when no rotation has happened this process
	// lifetime, or when the grace window has expired (readers
	// re-check prevExpiry every call).
	previous atomic.Pointer[string]

	// prevExpiry is the unix-nano deadline after which previous is
	// treated as nil. Stored separately so the atomic.Pointer swap
	// and the deadline write are both lock-free.
	prevExpiry atomic.Int64

	// source records where current came from — updated in Load and
	// Rotate. Guarded by the same atomic swap contract as current.
	source atomic.Pointer[Source]
}

// New constructs a Manager. Call Load() before the first read.
//
// tomlBearer is the value from configs/*.toml, used as the fallback
// when the DB has no override row yet. store persists rotations so
// they survive restart. logger is optional; nil silences audit logs.
func New(tomlBearer string, store ports.SettingsStore, logger *slog.Logger) *Manager {
	return &Manager{
		tomlBearer: tomlBearer,
		store:      store,
		logger:     logger,
	}
}

// Load initializes current from the DB (if a row exists) or falls
// back to the TOML value. Safe to call once at boot. Returns an
// error only if the store is unreachable; a missing row is not an
// error — that is the first-boot path.
func (m *Manager) Load(ctx context.Context) error {
	if m.store != nil {
		v, err := m.store.Get(ctx, SettingKey)
		if err == nil && v != "" {
			m.setCurrent(v, SourceDB)
			if m.logger != nil {
				m.logger.Info("admin bearer loaded from db",
					slog.String("last4", Last4(v)))
			}
			return nil
		}
		if err != nil && !errors.Is(err, domain.ErrSettingNotFound) {
			return fmt.Errorf("load admin bearer: %w", err)
		}
	}
	if m.tomlBearer == "" {
		// Nothing to serve. Callers will see Current()=="" and
		// BearerAuth will 500 — same behaviour as a missing TOML
		// admin_bearer today.
		empty := ""
		m.current.Store(&empty)
		src := SourceTOML
		m.source.Store(&src)
		return nil
	}
	m.setCurrent(m.tomlBearer, SourceTOML)
	if m.logger != nil {
		m.logger.Info("admin bearer loaded from toml",
			slog.String("last4", Last4(m.tomlBearer)))
	}
	return nil
}

// Current returns the bearer that BearerAuth should compare against
// for new requests. Empty string means the manager has not been
// loaded, or neither TOML nor DB supplied a value.
func (m *Manager) Current() string {
	p := m.current.Load()
	if p == nil {
		return ""
	}
	return *p
}

// CurrentSource reports where Current() was last populated from.
func (m *Manager) CurrentSource() Source {
	p := m.source.Load()
	if p == nil {
		return SourceTOML
	}
	return *p
}

// Accepts reports whether candidate matches either Current() or a
// still-in-grace previous bearer. Uses constant-time compare on both
// so timing attacks cannot distinguish the two paths.
//
// Empty candidates never match, even against an empty Current();
// unconfigured servers should reject anonymous callers.
func (m *Manager) Accepts(candidate string) bool {
	if candidate == "" {
		return false
	}
	if cur := m.Current(); cur != "" &&
		subtle.ConstantTimeCompare([]byte(candidate), []byte(cur)) == 1 {
		return true
	}
	if prev := m.previousIfActive(); prev != "" &&
		subtle.ConstantTimeCompare([]byte(candidate), []byte(prev)) == 1 {
		return true
	}
	return false
}

// previousIfActive returns the previous bearer if the grace window
// has not expired, or "" otherwise. Reads are lock-free: the pointer
// and the deadline are separate atomics, but Accepts only reads
// them once each and does not care about a torn race — an expired
// deadline discards the pointer, and a fresh Rotate wins next call.
func (m *Manager) previousIfActive() string {
	deadline := m.prevExpiry.Load()
	if deadline == 0 || time.Now().UnixNano() >= deadline {
		return ""
	}
	p := m.previous.Load()
	if p == nil {
		return ""
	}
	return *p
}

// Rotate swaps Current() to newBearer, moves the old value into the
// grace-window previous slot for GraceWindow, and persists the new
// value to the store so it survives restart.
//
// Returns ErrEmptyBearer / ErrBearerTooShort / ErrBearerWhitespace
// for malformed input; the caller (admin handler) turns those into
// 400 responses.
func (m *Manager) Rotate(ctx context.Context, newBearer string) error {
	if err := ValidateBearer(newBearer); err != nil {
		return err
	}
	if m.store != nil {
		if err := m.store.Set(ctx, SettingKey, newBearer); err != nil {
			return fmt.Errorf("persist admin bearer: %w", err)
		}
	}
	old := m.Current()
	if old != "" && old != newBearer {
		prev := old
		m.previous.Store(&prev)
		m.prevExpiry.Store(time.Now().Add(GraceWindow).UnixNano())
	} else {
		// No prior current, or a no-op rotate: clear any stale
		// grace window so Accepts stays honest.
		m.previous.Store(nil)
		m.prevExpiry.Store(0)
	}
	m.setCurrent(newBearer, SourceDB)
	if m.logger != nil {
		m.logger.Info("admin bearer rotated",
			slog.String("new_last4", Last4(newBearer)),
			slog.String("old_last4", Last4(old)),
			slog.Duration("grace", GraceWindow),
		)
	}
	return nil
}

// setCurrent updates current and source under a single logical swap.
// Not lock-based, but both writes are atomic and the readers tolerate
// interleaving (current is authoritative, source is metadata).
func (m *Manager) setCurrent(v string, src Source) {
	// Copy the string into a new heap-allocated value so the
	// pointer swap covers a stable payload. Passing &v directly
	// would work today but couples correctness to the caller's
	// stack lifetime.
	cur := v
	m.current.Store(&cur)
	s := src
	m.source.Store(&s)
}

// ValidateBearer enforces the shared server-side rules for a
// rotate: non-empty, at least MinBearerLength, no whitespace. The
// admin handler calls this before writing to the store so the DB
// never picks up a malformed value.
func ValidateBearer(s string) error {
	if s == "" {
		return ErrEmptyBearer
	}
	if len(s) < MinBearerLength {
		return ErrBearerTooShort
	}
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return ErrBearerWhitespace
		}
	}
	return nil
}

// Generate returns a fresh random bearer, 64 hex chars sourced from
// crypto/rand. Used when the operator picks the "generate" mode in
// the rotate dialog.
func Generate() (string, error) {
	var buf [GeneratedBearerBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate admin bearer: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Last4 returns the last four characters of a bearer for
// operator-visible previews. Bearers shorter than 4 chars surface
// as-is; empty bearers surface as "" so the admin GET can still
// distinguish "never set" from "".
func Last4(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
