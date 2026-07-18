// Package version exposes build-time metadata (semver tag, git commit, build
// time) injected via -ldflags at compile time. Runtime consumers should call
// Info() rather than reading the raw package variables so the dev-mode
// fallback (via runtime/debug.ReadBuildInfo) has a chance to fill in a
// vcs.revision / vcs.time when the binary was built from a working tree that
// still has a .git directory but no -X flags.
package version

import (
	"runtime/debug"
)

// These are set via -ldflags at build time. Example:
//
//	go build -ldflags "\
//	  -X github.com/greensheep999/higgsgo/internal/version.Version=v0.2.0 \
//	  -X github.com/greensheep999/higgsgo/internal/version.Commit=a1b2c3d \
//	  -X github.com/greensheep999/higgsgo/internal/version.BuildTime=2026-07-18T09:00:00Z"
//
// Left at zero-value sentinels ("dev", "none", "unknown") when the linker
// didn't inject anything (e.g. a bare `go build`). Info() layers a
// runtime/debug fallback on top so plain `go build` from a git tree still
// produces useful values.
var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

// BuildInfo is the resolved view of the build metadata, with dev-mode
// fallback applied. Callers should prefer this over reading the package
// globals so a binary built without -ldflags still reports the git commit /
// build time picked up by runtime/debug.ReadBuildInfo.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// Info returns the resolved BuildInfo. When Version == "dev" (i.e. -ldflags
// didn't inject a tag), it attempts to fill Commit / BuildTime from
// runtime/debug.ReadBuildInfo's vcs.* settings. Package globals are never
// mutated — the fallback is applied to a fresh struct so tests can assert on
// pristine defaults.
func Info() BuildInfo {
	info := BuildInfo{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
	}
	if Version != "dev" {
		return info
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" && info.Commit == "none" {
				// Truncate to short-sha for parity with the -X value the
				// release workflow injects (git rev-parse --short HEAD).
				if len(s.Value) > 7 {
					info.Commit = s.Value[:7]
				} else {
					info.Commit = s.Value
				}
			}
		case "vcs.time":
			if s.Value != "" && info.BuildTime == "unknown" {
				info.BuildTime = s.Value
			}
		}
	}
	return info
}

// IsDev reports whether this binary was built without an injected Version
// (i.e. from a plain `go build`, not a release build).
func IsDev() bool {
	return Version == "dev"
}
