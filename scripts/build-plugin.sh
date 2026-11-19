#!/usr/bin/env bash
# Stage the installable plugin tree, with the six binaries inside, into OUT.
# CI force-pushes OUT as the orphan "plugin" branch. Usage: scripts/build-plugin.sh OUT [VERSION]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:?usage: build-plugin.sh OUT [VERSION]}"
VERSION="${2:-$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
P="$ROOT/plugins/code-index-search"
rm -rf "$OUT"; mkdir -p "$OUT/.claude-plugin" "$OUT/hooks" "$OUT/bin" "$OUT/skills"
for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${t%/*}"; arch="${t#*/}"; ext=""; [ "$os" = windows ] && ext=".exe"
  ( cd "$ROOT" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$OUT/bin/code-index-$os-$arch$ext" ./cmd/code-index )
done
cp "$ROOT/scripts/plugin-launcher.sh" "$OUT/bin/code-index"
cp "$P/.claude-plugin/plugin.json" "$OUT/.claude-plugin/"
cp "$P/hooks/hooks.json" "$P/hooks/register.ts" "$OUT/hooks/"
mkdir -p "$OUT/skills/code-search" && cp "$ROOT/skills/code-search/SKILL.md" "$OUT/skills/code-search/"
cp "$ROOT/LICENSE" "$ROOT/README.md" "$OUT/"
chmod +x "$OUT"/bin/*
