#!/usr/bin/env bash
# Deployment helpers for the fixture workspace.

# build_release compiles the project and prints the artifact path.
build_release() {
  local target="$1"
  echo "building ${target}"
}

# deploy_release uploads the artifact for the given environment.
deploy_release() {
  local env="$1"
  build_release "$env"
  echo "deploying to ${env}"
}

function cleanup_temp {
  rm -rf "${TMPDIR:-/tmp}/deploy-fixture"
}

deploy_release "${1:-staging}"
cleanup_temp
