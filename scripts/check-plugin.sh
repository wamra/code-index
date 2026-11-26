#!/usr/bin/env bash
# Check a plugin tree staged by scripts/build-plugin.sh. Usage: scripts/check-plugin.sh OUT
set -euo pipefail
OUT="${1:?usage: check-plugin.sh OUT}"
fail=0
need() { [ -s "$OUT/$1" ] || { echo "missing or empty: $1" >&2; fail=1; }; }
for f in LICENSE THIRD_PARTY_NOTICES.md README.md .claude-plugin/plugin.json hooks/hooks.json hooks/register.ts \
  skills/code-search/SKILL.md bin/code-index \
  bin/code-index-darwin-amd64 bin/code-index-darwin-arm64 bin/code-index-linux-amd64 bin/code-index-linux-arm64 \
  bin/code-index-windows-amd64.exe bin/code-index-windows-arm64.exe; do
  need "$f"
done
# The notices must say which modules they cover, and must not hide a gap.
if [ -s "$OUT/THIRD_PARTY_NOTICES.md" ] && ! grep -q 'github.com/tetratelabs/wazero' "$OUT/THIRD_PARTY_NOTICES.md"; then
  echo "THIRD_PARTY_NOTICES.md does not list the Go modules; run scripts/gen-notices.sh" >&2; fail=1
fi
[ -x "$OUT/bin/code-index" ] || { echo "bin/code-index is not executable" >&2; fail=1; }
[ "$fail" = 0 ] && echo "plugin tree ok: $OUT"
exit "$fail"
