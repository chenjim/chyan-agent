#!/usr/bin/env bash
# Cross-compile and package chyan for linux/windows/darwin x amd64/arm64.
# Usage: scripts/release.sh [version]   (default: version const in chyan/cmd/chyan/main.go)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/chyan"
MAIN="$SRC/cmd/chyan"
DIST="$ROOT/dist"

# Version: $1 override, else the `version` const in main.go (single source of truth).
version="${1:-}"
if [[ -z "$version" ]]; then
  version="$(grep -Eo 'const version = "[^"]+"' "$MAIN/main.go" | grep -Eo '"[^"]+"' | tr -d '"')"
  [[ -n "$version" ]] || { echo "cannot extract version from $MAIN/main.go" >&2; exit 1; }
fi
echo "release version: $version"

# Build the PWA bundle first: the committed dist/index.html is only a
# placeholder, so a clean checkout would otherwise ship a broken Web UI.
# Set CHYAN_SKIP_WEB=1 only for CLI/TUI-only builds.
if [[ "${CHYAN_SKIP_WEB:-}" != "1" ]]; then
  echo "building web bundle ..."
  (cd "$ROOT/chyan/web" && npm ci && npm run build)
else
  echo "skipping web bundle (CHYAN_SKIP_WEB=1)"
fi

mkdir -p "$DIST"
# Idempotent re-runs: drop stale artifacts from previous versions.
find "$DIST" -mindepth 1 -maxdepth 1 \( -name 'chyan_*' -o -name 'SHA256SUMS' \) -exec rm -rf {} +

targets=(
  linux/amd64
  linux/arm64
  windows/amd64
  windows/arm64
  darwin/amd64
  darwin/arm64
)

for t in "${targets[@]}"; do
  os="${t%/*}"
  arch="${t#*/}"
  bin="chyan_${version}_${os}_${arch}"
  [[ "$os" == windows ]] && bin="${bin}.exe"
  echo "building $os/$arch ..."
  (cd "$SRC" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -o "$DIST/$bin" ./cmd/chyan)

  # Archive: tar.gz on unix, zip on windows.
  if [[ "$os" == windows ]]; then
    (cd "$DIST" && zip -q "${bin%.exe}.zip" "$bin")
  else
    tar -czf "$DIST/${bin}.tar.gz" -C "$DIST" "$bin"
  fi
done

# Checksums over all artifacts (binaries + archives).
(cd "$DIST" && sha256sum chyan_"$version"_* >SHA256SUMS)

echo
echo "artifacts in $DIST:"
ls -1 "$DIST"
