#!/usr/bin/env bash
# Log a message to stderr.
log() { printf '%s\n' "$*" >&2; }
build_release() {
  local name="$1"
  log "building $name"
}
