#!/usr/bin/env bash
before_break() { echo one; }
if [[ -n "$1" ]]; then
  before_break
after_fi() { echo two; }
after_fi
