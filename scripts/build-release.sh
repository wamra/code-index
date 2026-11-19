#!/usr/bin/env bash
# Cross-build release archives and checksums into dist/ with plain Go tooling.
# Usage: scripts/build-release.sh [VERSION]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="$ROOT/dist"
rm -rf "$OUT"; mkdir -p "$OUT"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"
  name="code-index_${VERSION}_${os}_${arch}"
  stage="$OUT/$name"; mkdir -p "$stage"
  exe=code-index; [ "$os" = windows ] && exe=code-index.exe
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$stage/$exe" ./cmd/code-index )
  cp "$ROOT/LICENSE" "$ROOT/README.md" "$stage/"
  cp "$ROOT/skills/code-search/SKILL.md" "$stage/SKILL.md"
  if [ "$os" = windows ]; then ( cd "$OUT" && zip -qr "$name.zip" "$name" ); else tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name"; fi
  rm -rf "$stage"
done
( cd "$OUT" && if command -v sha256sum >/dev/null 2>&1; then sha256sum *.tar.gz *.zip; else shasum -a 256 *.tar.gz *.zip; fi > checksums.txt )
ls -l "$OUT"
