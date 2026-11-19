#!/usr/bin/env bash
# Runs inside the smoke image. /src is the repo (read-only); code-index is on PATH.
set -uo pipefail
fail=0
check() { if eval "$2" >/dev/null 2>&1; then echo "PASS  $1"; else echo "FAIL  $1"; fail=1; fi; }

claude --version
echo "== marketplace"
claude plugin marketplace add /src
check "marketplace listed" "claude plugin marketplace list | grep -q code-index"

echo "== plugin"
claude plugin install code-index-search@code-index
check "plugin enabled" "claude plugin list | grep -A4 code-index-search | grep -q enabled"

echo "== claude -p"
if [ -z "${ANTHROPIC_API_KEY:-}${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
  echo "SKIP  claude -p: set ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN"
else
  mkdir -p ~/repo && cd ~/repo
  out=$(claude -p "Call the code-index-search search tool once with query Get-Widget and print its result verbatim." \
    --output-format stream-json --verbose --allowedTools "mcp__code-index-search__search" < /dev/null 2>&1)
  check "session lists the search tool" "grep -q mcp__code-index-search__search <<<\"\$out\""
  # No index exists here, so a wired-up tool answers with the binary's own message.
  check "tool reached the code-index binary" "grep -q 'run .code-index index' <<<\"\$out\""
  [ "$fail" = 0 ] || tail -c 1500 <<<"$out"
fi
exit "$fail"
