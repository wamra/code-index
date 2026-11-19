#!/usr/bin/env bash
# Logging helpers.

log_info() {
  printf 'info: %s\n' "$*"
}

log_error() {
  printf 'error: %s\n' "$*" >&2
}
