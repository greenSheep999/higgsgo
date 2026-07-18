package bearer

// Tests for the Manager surface: Load precedence (DB > TOML),
// Rotate semantics (grace window accepts prior bearer, expires
// after GraceWindow), and the ValidateBearer error cases surfaced
// by the admin rotate handler.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// memSettingsStore is an in-memory ports.SettingsStore stand-in
// used by every test in this file. Kept package-local so the store
// tests can live alongside the sqlite adapter without cross-testing
// coupling.
type memSettingsStore struct {
	mu   sync.Mutex
	data map[string]string
	ts   map[string]time.Time
	// getErr, if set, is returned from every Get call. Used to
	// simulate a store-outage during Load.
	getErr error
}

func newMemStore() *memSettingsStore {
	return &memSettingsStore{
		data: map[string]string{},
		ts:   map[string]time.Time{},
	}
}

func (m *memSettingsStore) Get(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return "", m.getErr
	}
	v, ok := m.data[key]
	if !ok {
		return "", domain.ErrSettingNotFound
	}
	return v, nil
}

func (m *memSettingsStore) Set(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	m.ts[key] = time.Now().UTC()
	return nil
}

func (m *memSettingsStore) UpdatedAt(_ context.Context, key string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.ts[key]
	if !ok {
		return time.Time{}, domain.ErrSettingNotFound
	}
	return t, nil
}

func TestManager_LoadFallsBackToTOML(t *testing.T) {
	store := newMemStore()
	mgr := New("dev-admin-token-abc", store, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := mgr.Current(); got != "dev-admin-token-abc" {
		t.Fatalf("current = %q, want the TOML value", got)
	}
	if src := mgr.CurrentSource(); src != SourceTOML {
		t.Fatalf("source = %q, want %q", src, SourceTOML)
	}
}

func TestManager_LoadPrefersDBOverride(t *testing.T) {
	store := newMemStore()
	if err := store.Set(context.Background(), SettingKey, "runtime-override-value"); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	mgr := New("dev-admin-token-abc", store, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := mgr.Current(); got != "runtime-override-value" {
		t.Fatalf("current = %q, want the DB override", got)
	}
	if src := mgr.CurrentSource(); src != SourceDB {
		t.Fatalf("source = %q, want %q", src, SourceDB)
	}
}

func TestManager_AcceptsRejectsEmpty(t *testing.T) {
	mgr := New("dev-admin-token-abc", newMemStore(), nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if mgr.Accepts("") {
		t.Fatalf("Accepts(\"\") returned true; empty candidates must never match")
	}
	if !mgr.Accepts("dev-admin-token-abc") {
		t.Fatalf("Accepts(current) returned false")
	}
	if mgr.Accepts("some-other-value") {
		t.Fatalf("Accepts(unknown) returned true")
	}
}

func TestManager_RotateGraceWindowAcceptsPrevious(t *testing.T) {
	mgr := New("dev-admin-token-abc", newMemStore(), nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.Rotate(context.Background(), "new-admin-token-9999"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !mgr.Accepts("new-admin-token-9999") {
		t.Fatalf("Accepts(new) false; expected true after rotate")
	}
	if !mgr.Accepts("dev-admin-token-abc") {
		t.Fatalf("Accepts(previous) false inside grace window; expected true")
	}
	// Force the grace window to expire.
	mgr.prevExpiry.Store(time.Now().Add(-time.Second).UnixNano())
	if mgr.Accepts("dev-admin-token-abc") {
		t.Fatalf("Accepts(previous) true after grace expired; expected false")
	}
	if !mgr.Accepts("new-admin-token-9999") {
		t.Fatalf("Accepts(new) false after grace expired; still expected true")
	}
}

func TestManager_RotatePersistsToStore(t *testing.T) {
	store := newMemStore()
	mgr := New("dev-admin-token-abc", store, nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.Rotate(context.Background(), "post-rotation-value"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	v, err := store.Get(context.Background(), SettingKey)
	if err != nil {
		t.Fatalf("store lookup: %v", err)
	}
	if v != "post-rotation-value" {
		t.Fatalf("store value = %q, want %q", v, "post-rotation-value")
	}
	// A fresh Manager pointed at the same store must see the DB value.
	mgr2 := New("dev-admin-token-abc", store, nil)
	if err := mgr2.Load(context.Background()); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got := mgr2.Current(); got != "post-rotation-value" {
		t.Fatalf("current after restart = %q, want persisted %q", got, "post-rotation-value")
	}
	if src := mgr2.CurrentSource(); src != SourceDB {
		t.Fatalf("source after restart = %q, want %q", src, SourceDB)
	}
}

func TestManager_RotateRejectsMalformed(t *testing.T) {
	mgr := New("dev-admin-token-abc", newMemStore(), nil)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mgr.Rotate(context.Background(), ""); !errors.Is(err, ErrEmptyBearer) {
		t.Fatalf("empty: got %v, want ErrEmptyBearer", err)
	}
	if err := mgr.Rotate(context.Background(), "short"); !errors.Is(err, ErrBearerTooShort) {
		t.Fatalf("short: got %v, want ErrBearerTooShort", err)
	}
	if err := mgr.Rotate(context.Background(), "has spaces in it here"); !errors.Is(err, ErrBearerWhitespace) {
		t.Fatalf("whitespace: got %v, want ErrBearerWhitespace", err)
	}
	// A malformed rotate must not alter Current().
	if got := mgr.Current(); got != "dev-admin-token-abc" {
		t.Fatalf("current changed after malformed rotate: got %q", got)
	}
}

func TestManager_GenerateProducesHex(t *testing.T) {
	tok, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(tok) != GeneratedBearerBytes*2 {
		t.Fatalf("token len = %d, want %d", len(tok), GeneratedBearerBytes*2)
	}
	for _, r := range tok {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("token contains non-hex char %q", r)
		}
	}
	// Generated tokens are long enough for the validator to accept.
	if err := ValidateBearer(tok); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestManager_Last4(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"abcd", "abcd"},
		{"abcde", "bcde"},
		{"dev-admin-token-abc", "-abc"},
	}
	for _, c := range cases {
		if got := Last4(c.in); got != c.want {
			t.Fatalf("Last4(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestManager_LoadPropagatesStoreOutage(t *testing.T) {
	store := newMemStore()
	store.getErr = errors.New("db unreachable")
	mgr := New("dev-admin-token-abc", store, nil)
	if err := mgr.Load(context.Background()); err == nil {
		t.Fatalf("load succeeded with a failing store; expected error")
	} else if !strings.Contains(err.Error(), "db unreachable") {
		t.Fatalf("error %q missing store cause", err)
	}
}
