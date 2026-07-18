package version

import (
	"testing"
)

// TestInfo_DevDefaults verifies the pristine dev-mode defaults. When Version
// has not been overridden by -ldflags we expect the sentinel triple; the
// vcs.* fallback may fill Commit / BuildTime in when tests run inside a git
// checkout, so we assert on Version only (the one field that's guaranteed
// stable across test environments).
func TestInfo_DevDefaults(t *testing.T) {
	// Save + restore in case another test set them (parallel safety).
	origV, origC, origB := Version, Commit, BuildTime
	t.Cleanup(func() {
		Version = origV
		Commit = origC
		BuildTime = origB
	})

	Version = "dev"
	Commit = "none"
	BuildTime = "unknown"

	got := Info()
	if got.Version != "dev" {
		t.Errorf("Version: got %q want dev", got.Version)
	}
	// Commit / BuildTime may be filled from runtime/debug when we're
	// running inside a git checkout — that's the intended fallback. We
	// only assert they are non-empty strings (never the raw \"\" that
	// would indicate a bug in the fallback path).
	if got.Commit == "" {
		t.Errorf("Commit is empty; fallback should have kept the sentinel or filled it")
	}
	if got.BuildTime == "" {
		t.Errorf("BuildTime is empty; fallback should have kept the sentinel or filled it")
	}
}

// TestInfo_InjectedValuesShortCircuit verifies that when -ldflags injects a
// non-dev Version, Info() does NOT fall back to runtime/debug (so a release
// binary reports exactly the trio the linker embedded, even if the checkout
// it was built from had a newer HEAD than the tag).
func TestInfo_InjectedValuesShortCircuit(t *testing.T) {
	origV, origC, origB := Version, Commit, BuildTime
	t.Cleanup(func() {
		Version = origV
		Commit = origC
		BuildTime = origB
	})

	Version = "v0.2.0"
	Commit = "abcdef1"
	BuildTime = "2026-07-18T09:00:00Z"

	got := Info()
	if got.Version != "v0.2.0" {
		t.Errorf("Version: got %q want v0.2.0", got.Version)
	}
	if got.Commit != "abcdef1" {
		t.Errorf("Commit: got %q want abcdef1 (fallback should not run)", got.Commit)
	}
	if got.BuildTime != "2026-07-18T09:00:00Z" {
		t.Errorf("BuildTime: got %q want 2026-07-18T09:00:00Z (fallback should not run)", got.BuildTime)
	}
}

// TestIsDev checks the helper. Simple but keeps callers from having to
// remember the sentinel string.
func TestIsDev(t *testing.T) {
	origV := Version
	t.Cleanup(func() { Version = origV })

	Version = "dev"
	if !IsDev() {
		t.Errorf("IsDev() = false; want true when Version == dev")
	}
	Version = "v0.1.0"
	if IsDev() {
		t.Errorf("IsDev() = true; want false when Version is a tag")
	}
}
