#!/usr/bin/env bash
# Run the Linux end-to-end test (test/docker/e2e.sh) in a clean container.
#
# Usage: scripts/test-docker.sh [--pwsh-plugin DIR] [--no-build] [--shell]
#   --pwsh-plugin DIR  mount a powershell-lsp plugin directory (it holds
#                      scripts/start-pses.ps1) read-only at /plugins/powershell-lsp;
#                      enables the PowerShell Editor Services steps
#   --no-build         reuse the existing code-index-test:latest image
#   --shell            open a shell in the container instead of running the test
#
# The repository is mounted read-only at /src; the test works on a copy.
# ANTHROPIC_API_KEY and CLAUDE_CODE_OAUTH_TOKEN are passed through only when
# they are already set in this shell. Ollama on the host is reached through
# host.docker.internal:11434 (the embeddings step skips when it is absent).
set -euo pipefail

IMAGE="code-index-test:latest"
BUILD=1
SHELL_MODE=0
PWSH_PLUGIN=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --pwsh-plugin) PWSH_PLUGIN="${2:?--pwsh-plugin needs a directory}"; shift 2 ;;
    --no-build) BUILD=0; shift ;;
    --shell) SHELL_MODE=1; shift ;;
    -h|--help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "the Docker daemon is not running" >&2; exit 1; }

if [ "$BUILD" = 1 ]; then
  # Pull the base images first: the builder alone can fail to authorize against
  # the registry on some networks, while a plain pull works.
  go_ver="$(sed -n 's/^ARG GO_VERSION=//p' "$ROOT/test/docker/Dockerfile")"
  node_img="$(sed -n 's/^ARG NODE_IMAGE=//p' "$ROOT/test/docker/Dockerfile")"
  docker pull "golang:${go_ver}-trixie" >/dev/null 2>&1 || echo "NOTE: could not pull golang:${go_ver}-trixie; using the local copy if any" >&2
  docker pull "$node_img" >/dev/null 2>&1 || echo "NOTE: could not pull $node_img; using the local copy if any" >&2
  docker build -t "$IMAGE" "$ROOT/test/docker"
fi

args=(--rm --add-host host.docker.internal:host-gateway -v "$ROOT:/src:ro")
if [ -n "$PWSH_PLUGIN" ]; then
  [ -f "$PWSH_PLUGIN/scripts/start-pses.ps1" ] || { echo "$PWSH_PLUGIN/scripts/start-pses.ps1 not found" >&2; exit 2; }
  args+=(-v "$(cd "$PWSH_PLUGIN" && pwd):/plugins/powershell-lsp:ro")
fi
[ -n "${ANTHROPIC_API_KEY:-}" ] && args+=(-e ANTHROPIC_API_KEY)
[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && args+=(-e CLAUDE_CODE_OAUTH_TOKEN)

if [ "$SHELL_MODE" = 1 ]; then
  exec docker run -it "${args[@]}" "$IMAGE" bash
fi
exec docker run "${args[@]}" "$IMAGE" bash /src/test/docker/e2e.sh
