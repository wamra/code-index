#!/usr/bin/env bash
source ./lib/hooks.sh
on_start() { echo starting; }
for f in plugins/*.sh; do
  . "$f"
done
run_hooks
plugin_a
