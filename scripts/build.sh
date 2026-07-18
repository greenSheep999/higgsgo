#!/usr/bin/env bash
# Build higgsgo + higgsgo-cli locally with git version metadata baked in.
#
# Mirrors the -ldflags trio the release workflow injects, so /admin/version
# and the WebUI sidebar footer show a real build ID even for local binaries.
#
# Usage:
#   ./scripts/build.sh              # infers everything from git
#   ./scripts/build.sh v0.3.0-dev   # override the semver tag
#
# Env overrides:
#   VERSION      overrides the semver tag (default: git tag or "dev")
#   COMMIT       overrides the short sha (default: `git rev-parse --short HEAD`)
#   BUILD_TIME   overrides the build timestamp (default: ISO-8601 UTC now)
#   OUT_DIR      output directory (default: ./bin)

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

MODULE="github.com/greensheep999/higgsgo"

VERSION="${1:-${VERSION:-}}"
if [[ -z "${VERSION}" ]]; then
  # Prefer an exact tag on HEAD; fall back to the closest tag with `-N-gsha`;
  # fall back to "dev" when there are no tags at all (fresh repo).
  if VERSION="$(git describe --tags --exact-match 2>/dev/null)"; then
    :
  elif VERSION="$(git describe --tags --always 2>/dev/null)"; then
    :
  else
    VERSION="dev"
  fi
fi

COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%FT%TZ)}"
OUT_DIR="${OUT_DIR:-./bin}"

mkdir -p "$OUT_DIR"

LDFLAGS="-s -w \
  -X ${MODULE}/internal/version.Version=${VERSION} \
  -X ${MODULE}/internal/version.Commit=${COMMIT} \
  -X ${MODULE}/internal/version.BuildTime=${BUILD_TIME}"

echo "building higgsgo (version=${VERSION} commit=${COMMIT} time=${BUILD_TIME})"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT_DIR/higgsgo"     ./cmd/higgsgo
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$OUT_DIR/higgsgo-cli" ./cmd/higgsgo-cli

echo "wrote:"
ls -lh "$OUT_DIR/higgsgo" "$OUT_DIR/higgsgo-cli"
