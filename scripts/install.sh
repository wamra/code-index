#!/usr/bin/env bash
# Install code-index from a local checkout (macOS or Linux).
# Builds the binary with Go, puts it in ~/.local/bin, writes a starter config
# when none exists, installs the code-search skill and the code-index-search
# mod (model tools and /code in Claude Code), then runs doctor.
# Usage: scripts/install.sh [--bin-dir DIR] [--no-skill] [--no-mod]
set -euo pipefail

BIN_DIR="${HOME}/.local/bin"
SKILL=1
MOD=1
while [ "$#" -gt 0 ]; do
  case "$1" in
    --bin-dir) BIN_DIR="$2"; shift 2 ;;
    --no-skill) SKILL=0; shift ;;
    --no-mod) MOD=0; shift ;;
    -h|--help) sed -n '2,6p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v go >/dev/null 2>&1 || { echo "Go is required to build from source: https://go.dev/dl/" >&2; exit 1; }

mkdir -p "$BIN_DIR"
echo "== building code-index"
( cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)" -o "$BIN_DIR/code-index.new" ./cmd/code-index )
mv -f "$BIN_DIR/code-index.new" "$BIN_DIR/code-index"
echo "installed $BIN_DIR/code-index ($("$BIN_DIR/code-index" version))"

case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "NOTE: $BIN_DIR is not on PATH; add it to your shell profile." ;; esac

CONFIG="${CODE_INDEX_CONFIG:-${HOME}/.config/code-index/config.json}"
if [ ! -f "$CONFIG" ]; then
  echo "== writing a starter config"
  "$BIN_DIR/code-index" config init
fi

if [ "$SKILL" = 1 ]; then
  echo "== installing the code-search skill"
  "$BIN_DIR/code-index" install-skill
fi
if [ "$MOD" = 1 ]; then
  if command -v claude >/dev/null 2>&1; then
    echo "== installing the code-index-search mod"
    # Registering the checkout as a marketplace is repeatable: an existing
    # entry is refreshed instead.
    claude plugin marketplace add "$ROOT" || claude plugin marketplace update code-index
    claude plugin install code-index-search@code-index
    echo "restart Claude Code to load the search and show tools and /code"
  else
    echo "NOTE: claude is not on PATH; skipped the code-index-search mod (see plugins/code-index-search/README.md)."
  fi
fi
echo "== doctor"
"$BIN_DIR/code-index" doctor || true
echo
echo "Next: cd <repo> && code-index index    (index a repo; embeddings: code-index index --embed)"
