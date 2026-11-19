#!/usr/bin/env bash
# Smoke test on a clean consumer image (Claude Code only): add the marketplace,
# install the plugin, and, when a token is set, call the tool through claude -p.
# Usage: scripts/smoke.sh        (token: ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN)
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARCH="$(docker version --format '{{.Server.Arch}}')"
mkdir -p "$ROOT/dist/smoke"
( cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o dist/smoke/code-index ./cmd/code-index )
docker pull -q debian:trixie-slim >/dev/null 2>&1 || true
docker build -q -t code-index-smoke "$ROOT/test/smoke" >/dev/null
docker run --rm -e ANTHROPIC_API_KEY -e CLAUDE_CODE_OAUTH_TOKEN \
  -v "$ROOT:/src:ro" -v "$ROOT/dist/smoke/code-index:/usr/local/bin/code-index:ro" \
  code-index-smoke bash /src/test/smoke/smoke.sh
