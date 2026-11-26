#!/usr/bin/env bash
# Install code-index (macOS or Linux), from a release archive or a checkout.
# Puts the binary in ~/.local/bin, writes a starter config when none exists,
# installs the code-search skill and the code-index-search mod (model tools and
# /code in Claude Code), then checks the setup.
#
# Release archive: run ./install.sh from the extracted directory; the prebuilt
# binary next to it is installed and Go is not needed. The mod is copied to
# ${XDG_DATA_HOME:-~/.local/share}/code-index/marketplace, so the extracted
# directory can be deleted afterwards.
# Source checkout: scripts/install.sh builds the binary with Go and registers the
# checkout itself as the marketplace.
#
# The binary goes to --bin-dir and a starter config is written only when none
# exists. Nothing under ~/.claude is touched (the skill, the mod registration)
# unless you say so: pass --skill and/or --mod, pass --yes, or answer the
# prompts in an interactive terminal. An existing, different skill file or an
# existing marketplace registration is left alone unless --force is given.
# --dry-run prints what would change and changes nothing.
#
# Usage: install.sh [--binary FILE] [--bin-dir DIR] [--skill|--no-skill]
#                   [--mod|--no-mod] [--yes] [--force] [--dry-run]
set -euo pipefail

BIN_DIR="${HOME}/.local/bin"
BINARY=""
SKILL=ask
MOD=ask
YES=0
FORCE=0
DRY=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --bin-dir) BIN_DIR="$2"; shift 2 ;;
    --skill) SKILL=1; shift ;;
    --no-skill) SKILL=0; shift ;;
    --mod) MOD=1; shift ;;
    --no-mod) MOD=0; shift ;;
    --yes|-y) YES=1; shift ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Archive layout: the script, the binary and .claude-plugin/ share a directory.
# Source layout: the script is in scripts/ and the repository root is its parent.
if [ -z "$BINARY" ] && [ -f "$HERE/code-index" ]; then BINARY="$HERE/code-index"; fi
if [ -f "$HERE/.claude-plugin/marketplace.json" ]; then ROOT="$HERE"; ARCHIVE=1; else ROOT="$(cd "$HERE/.." && pwd)"; ARCHIVE=0; fi

CONFIG="${CODE_INDEX_CONFIG:-${HOME}/.config/code-index/config.json}"
SKILL_FILE="${HOME}/.claude/skills/code-search/SKILL.md"
MKT_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/code-index/marketplace"

# confirm WHAT MODE: 0 = no, 1 = yes, ask = prompt when a terminal is attached.
confirm() {
  if [ "$2" = 1 ] || { [ "$2" = ask ] && [ "$YES" = 1 ]; }; then return 0; fi
  if [ "$2" = 0 ]; then return 1; fi
  if [ -t 0 ] && [ -t 1 ]; then
    printf '%s [y/N] ' "$1"; read -r reply || reply=""
    case "$reply" in y|Y|yes|YES) return 0 ;; esac
    return 1
  fi
  echo "skipped: $1 (not interactive; pass --yes or the explicit flag to do it)"
  return 1
}

echo "This installer will change:"
echo "  - $BIN_DIR/code-index (the binary)"
[ -f "$CONFIG" ] && echo "  - $CONFIG exists and is left as it is" || echo "  - $CONFIG (a new starter config)"
case "$SKILL" in 0) ;; *) echo "  - $SKILL_FILE (the code-search skill, under ~/.claude; asks first unless --skill or --yes)" ;; esac
case "$MOD" in 0) ;; *) echo "  - the Claude Code marketplace and plugin registration for code-index-search (under ~/.claude; asks first unless --mod or --yes)" ;; esac
if [ "$DRY" = 1 ]; then echo "dry run: nothing was changed."; exit 0; fi

mkdir -p "$BIN_DIR"
if [ -n "$BINARY" ]; then
  [ -f "$BINARY" ] || { echo "binary not found: $BINARY" >&2; exit 1; }
  echo "== installing the prebuilt binary"
  cp "$BINARY" "$BIN_DIR/code-index.new"
  chmod 0755 "$BIN_DIR/code-index.new"
else
  command -v go >/dev/null 2>&1 || { echo "Go is required to build from source (or use a release archive): https://go.dev/dl/" >&2; exit 1; }
  echo "== building code-index"
  ( cd "$ROOT" && CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)" -o "$BIN_DIR/code-index.new" ./cmd/code-index )
fi
mv -f "$BIN_DIR/code-index.new" "$BIN_DIR/code-index"
echo "installed $BIN_DIR/code-index ($("$BIN_DIR/code-index" version))"

case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "NOTE: $BIN_DIR is not on PATH; add it to your shell profile." ;; esac

if [ ! -f "$CONFIG" ]; then
  echo "== writing a starter config"
  "$BIN_DIR/code-index" config init
fi

if confirm "Install the code-search skill into ~/.claude/skills?" "$SKILL"; then
  echo "== installing the code-search skill"
  if [ "$FORCE" = 1 ]; then "$BIN_DIR/code-index" install-skill --force; else "$BIN_DIR/code-index" install-skill || echo "NOTE: the existing skill was kept (use --force to replace it)."; fi
fi
if confirm "Register the code-index-search plugin with Claude Code (changes ~/.claude)?" "$MOD"; then
  if command -v claude >/dev/null 2>&1; then
    echo "== installing the code-index-search mod"
    MKT="$ROOT"
    if [ "$FORCE" != 1 ] && claude plugin marketplace list 2>/dev/null | grep -q 'code-index'; then
      echo "NOTE: a code-index marketplace is already registered; left as it is (use --force to refresh it)."
      MOD_SKIP=1
    fi
    if [ "${MOD_SKIP:-0}" != 1 ] && [ "$ARCHIVE" = 1 ]; then
      # A stable per-user copy, so the extracted archive can be deleted. The old
      # copy is swapped out by renames; an upgrade replaces it entirely.
      MKT="$MKT_DIR"
      mkdir -p "$(dirname "$MKT")"
      rm -rf "$MKT.new" "$MKT.old"
      mkdir -p "$MKT.new/.claude-plugin"
      cp "$ROOT/.claude-plugin/marketplace.json" "$MKT.new/.claude-plugin/"
      cp -R "$ROOT/plugins" "$MKT.new/plugins"
      [ -d "$MKT" ] && mv "$MKT" "$MKT.old"
      mv "$MKT.new" "$MKT"
      rm -rf "$MKT.old"
    fi
    # Registering the directory is repeatable: when it is already registered
    # the marketplace is refreshed instead.
    if [ "${MOD_SKIP:-0}" != 1 ]; then
      claude plugin marketplace add "$MKT" || claude plugin marketplace update code-index
      claude plugin install code-index-search@code-index --config "binary=$BIN_DIR/code-index"
      echo "restart Claude Code to load the search and show tools and /code"
    fi
  else
    echo "NOTE: claude is not on PATH; skipped the code-index-search mod (see plugins/code-index-search/README.md)."
  fi
fi
echo "== health check (configuration, language servers, embedder)"
"$BIN_DIR/code-index" doctor --no-index || true
echo
echo "Next: cd <repo> && code-index index    (index a repo; embeddings: code-index index --embed)"
