#!/usr/bin/env bash
source ./lib/util.sh
alias br=build_release
deploy() {
  local name="$1"
  build_release "$name"
  br "$name"
  local cmd=build_release
  "$cmd" x
  eval "build_release y"
  trap 'log bye' EXIT
}
other() {
  local name=zzz
  echo "$name"
}
unquoted() {
  local f=$1
  rm $f
}
deploy app
