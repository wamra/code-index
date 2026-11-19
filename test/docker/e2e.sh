#!/usr/bin/env bash
# End-to-end test of code-index on a clean Linux machine. Runs INSIDE the
# code-index-test image (see Dockerfile and scripts/test-docker.sh), with the
# repository mounted read-only at /src. Every step prints PASS, FAIL or
# SKIP <reason>; the exit status is non-zero when any step FAILED.
#
# Optional mounts and environment:
#   /plugins/powershell-lsp          PowerShell Editor Services launcher plugin
#   host.docker.internal:11434       Ollama with embeddinggemma
#   ANTHROPIC_API_KEY | CLAUDE_CODE_OAUTH_TOKEN   enables the model-in-the-loop step
set -uo pipefail

SRC="${SRC:-/src}"
WORK="${WORK:-/work}"
REPO="$WORK/repo"
LOG="$WORK/logs"
PS_PLUGIN="${PS_PLUGIN:-/plugins/powershell-lsp}"
OLLAMA_URL="${OLLAMA_URL:-http://host.docker.internal:11434}"
export PATH="$HOME/.local/bin:$PATH"

FAILS=0
PASSES=0
SKIPS=0

pass() { PASSES=$((PASSES + 1)); printf 'PASS  %s\n' "$1"; }
fail() { FAILS=$((FAILS + 1)); printf 'FAIL  %s\n' "$1"; }
skip() { SKIPS=$((SKIPS + 1)); printf 'SKIP  %s: %s\n' "$1" "$2"; }
note() { printf '      %s\n' "$*"; }
# show the first lines of a file as indented evidence
evidence() { [ -f "$1" ] && head -n "${2:-12}" "$1" | sed 's/^/      | /'; }

# check NAME CMD...: runs CMD, output to $LOG/NAME.log, PASS when it exits 0.
check() {
  local name="$1"; shift
  local out="$LOG/${name//[^A-Za-z0-9._-]/_}.log"
  if "$@" >"$out" 2>&1; then pass "$name"; else fail "$name"; evidence "$out" 25; fi
}
# assert NAME CONDITION-COMMAND...: silent check, FAIL prints nothing extra.
assert() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then pass "$name"; else fail "$name"; fi
}
# contains FILE REGEX
contains() { grep -Eq -- "$2" "$1"; }

mkdir -p "$LOG"
echo "== environment"
{
  echo "uname:      $(uname -sm)"
  echo "home:       $HOME (user $(id -un))"
  echo "go:         $(go version)"
  echo "node:       $(node --version)"
  echo "claude:     $(claude --version 2>&1 | head -1)"
  echo "pwsh:       $(pwsh -NoLogo -NoProfile -Command '$PSVersionTable.PSVersion.ToString()' 2>&1)"
  echo "shellcheck: $(shellcheck --version | sed -n 2p)"
  echo "bash-language-server: $(bash-language-server --version 2>&1 | head -1)"
} | sed 's/^/  /'

# The repo is mounted read-only: work on a writable copy owned by this user.
rm -rf "$REPO"
mkdir -p "$WORK"
cp -R "$SRC" "$REPO"
chmod -R u+w "$REPO"
cd "$REPO" || exit 2

# mkrepo FIXTURE DIR: a fresh git repo with a copy of testdata/FIXTURE.
mkrepo() {
  rm -rf "$2"
  mkdir -p "$2"
  cp -R "$REPO/testdata/$1/." "$2/"
  git -C "$2" init -q
}

# ---------------------------------------------------------------- a. go
echo "== a. go vet, go test"
check "a1 go vet ./..." go vet ./...
check "a2 go test ./..." go test ./...

# ---------------------------------------------------------------- b. install
echo "== b. scripts/install.sh"
CFG="$HOME/.config/code-index/config.json"
check "b1 install.sh (first run)" scripts/install.sh
assert "b2 binary in ~/.local/bin" test -x "$HOME/.local/bin/code-index"
check "b3 code-index version" code-index version
note "version: $(code-index version 2>&1)"
assert "b4 starter config written" test -f "$CFG"
assert "b5 config lists bash (server on PATH)" jq -e '.languages.bash.command == "bash-language-server"' "$CFG"
note "config languages: $(jq -r '.languages | keys | join(",")' "$CFG" 2>&1)"
assert "b6 skill file exists" test -s "$HOME/.claude/skills/code-search/SKILL.md"
claude plugin marketplace list >"$LOG/b-marketplace.log" 2>&1
if contains "$LOG/b-marketplace.log" 'code-index'; then pass "b7 marketplace list shows code-index"; else fail "b7 marketplace list shows code-index"; evidence "$LOG/b-marketplace.log"; fi
claude plugin list --json >"$LOG/b-plugins.json" 2>&1
if jq -e 'map(select(.id == "code-index-search@code-index" and .enabled == true)) | length == 1' "$LOG/b-plugins.json" >/dev/null 2>&1; then
  pass "b8 plugin list: code-index-search enabled"
else fail "b8 plugin list: code-index-search enabled"; evidence "$LOG/b-plugins.json"; fi
if [ "$(jq -r '.pluginConfigs["code-index-search@code-index"].options.binary' "$HOME/.claude/settings.json" 2>/dev/null)" = "$HOME/.local/bin/code-index" ]; then
  pass "b9 settings.json has the binary option"
else fail "b9 settings.json has the binary option"; evidence "$HOME/.claude/settings.json" 20; fi
note "binary option: $(jq -c '.pluginConfigs' "$HOME/.claude/settings.json" 2>&1)"
# repeatable: a second run must succeed and leave the same state
cp "$CFG" "$LOG/config.before.json"
check "b10 install.sh (second run)" scripts/install.sh
assert "b11 second run kept the config" cmp -s "$CFG" "$LOG/config.before.json"
claude plugin list --json >"$LOG/b-plugins2.json" 2>&1
assert "b12 still one enabled plugin after the second run" jq -e 'map(select(.id == "code-index-search@code-index" and .enabled == true)) | length == 1' "$LOG/b-plugins2.json"
claude plugin marketplace list >"$LOG/b-marketplace2.log" 2>&1
assert "b13 marketplace listed once after the second run" test "$(grep -c '❯ code-index$' "$LOG/b-marketplace2.log")" = 1

# ---------------------------------------------------------------- c. plugin tooling
echo "== c. claude plugin validate / test"
check "c1 validate plugin" claude plugin validate plugins/code-index-search
check "c2 validate marketplace" claude plugin validate .
check "c3 plugin test" claude plugin test plugins/code-index-search
note "$(grep -E '^ *[0-9]+ (pass|fail)' "$LOG/c3_plugin_test.log" | tr -s ' ' | tr '\n' ' ')"

# ---------------------------------------------------------------- d. bash-language-server
echo "== d. real bash-language-server on testdata/fixture-sh"
SH="$WORK/fixture-sh"
mkrepo fixture-sh "$SH"
cd "$SH" || exit 2
check "d1 index" code-index index
evidence "$LOG/d1_index.log" 8
assert "d2 .Wammy/code.sqlite exists" test -s .Wammy/code.sqlite
assert "d3 .Wammy/ in .git/info/exclude" grep -qx '\.Wammy/' .git/info/exclude
git status --short >"$LOG/d-status.log" 2>&1
if grep -q 'Wammy' "$LOG/d-status.log"; then fail "d4 git status does not list .Wammy"; evidence "$LOG/d-status.log"; else pass "d4 git status does not list .Wammy"; fi
code-index search deploy_release >"$LOG/d-search.log" 2>&1
first="$(grep -m1 -E '^[^ ]' "$LOG/d-search.log")"
if printf '%s' "$first" | grep -q 'deploy_release'; then pass "d5 search deploy_release returns it first"; else fail "d5 search deploy_release returns it first"; fi
evidence "$LOG/d-search.log" 6
check "d6 show deploy_release" code-index show deploy_release --source
if contains "$LOG/d6_show_deploy_release.log" 'deploy\.sh:11' && contains "$LOG/d6_show_deploy_release.log" 'build_release "\$env"'; then pass "d6b show prints the location and the source"; else fail "d6b show prints the location and the source"; evidence "$LOG/d6_show_deploy_release.log" 20; fi
code-index doctor >"$LOG/d-doctor.log" 2>&1
if grep -q '^FAIL' "$LOG/d-doctor.log"; then fail "d7 doctor has no FAIL"; evidence "$LOG/d-doctor.log" 20; else pass "d7 doctor has no FAIL"; fi
evidence "$LOG/d-doctor.log" 12
if ls .Wammy | grep -Eq -- '-(wal|shm)$'; then fail "d8 no -wal/-shm left behind"; ls -la .Wammy | sed 's/^/      | /'; else pass "d8 no -wal/-shm left behind"; fi
ls -la .Wammy | sed 's/^/      | /'

# ---------------------------------------------------------------- e. PowerShell Editor Services
echo "== e. real PowerShell Editor Services on testdata/fixture-ps"
PSOUT_FIRST=""
if [ ! -f "$PS_PLUGIN/scripts/start-pses.ps1" ]; then
  skip "e PowerShell Editor Services" "$PS_PLUGIN not mounted (use scripts/test-docker.sh --pwsh-plugin DIR)"
else
  # First run downloads PSES from GitHub; do it here so a blocked download is a SKIP.
  PSOUT_FIRST="$(timeout 240 pwsh -NoLogo -NoProfile -File "$PS_PLUGIN/scripts/ensure-pses.ps1" 2>&1)"
  note "ensure-pses.ps1 (first run): $PSOUT_FIRST"
  if ! printf '%s' "$PSOUT_FIRST" | grep -Eq 'ready|installed to'; then
    skip "e PowerShell Editor Services" "PSES download failed or blocked (see ensure-pses output above)"
  else
    jq --arg s "$PS_PLUGIN/scripts/start-pses.ps1" '.languages.powershell = {
        command: "pwsh",
        args: ["-NoLogo", "-NoProfile", "-File", $s],
        extensions: [".ps1", ".psm1", ".psd1"],
        language_id: "powershell",
        startup_timeout_s: 120
      }' "$CFG" >"$CFG.new" && mv "$CFG.new" "$CFG"
    PS="$WORK/fixture-ps"
    mkrepo fixture-ps "$PS"
    cd "$PS" || exit 2
    check "e1 index" code-index index
    evidence "$LOG/e1_index.log" 8
    code-index search "Get-Widget" >"$LOG/e-search.log" 2>&1
    first="$(grep -m1 -E '^[^ ]' "$LOG/e-search.log")"
    if printf '%s' "$first" | grep -q 'Widgets/Widgets.psm1'; then pass "e2 search Get-Widget returns Widgets/Widgets.psm1 first"; else fail "e2 search Get-Widget returns Widgets/Widgets.psm1 first"; fi
    evidence "$LOG/e-search.log" 6
    code-index doctor >"$LOG/e-doctor.log" 2>&1
    if grep -q '^FAIL' "$LOG/e-doctor.log"; then fail "e3 doctor has no FAIL"; evidence "$LOG/e-doctor.log" 20; else pass "e3 doctor has no FAIL"; fi
    if ls .Wammy | grep -Eq -- '-(wal|shm)$'; then fail "e4 no -wal/-shm left behind"; else pass "e4 no -wal/-shm left behind"; fi
  fi
fi
cd "$REPO" || exit 2

# ---------------------------------------------------------------- f. bonus: powershell-lsp plugin report
echo "== f. report only: powershell-lsp plugin on this pwsh"
if [ ! -f "$PS_PLUGIN/scripts/ensure-pses.ps1" ]; then
  skip "f powershell-lsp report" "$PS_PLUGIN not mounted"
else
  echo "  pwsh PSVersion: $(pwsh -NoLogo -NoProfile -Command '$PSVersionTable.PSVersion.ToString()' 2>&1)"
  echo "  ensure-pses.ps1 output (verbatim):"
  timeout 240 pwsh -NoLogo -NoProfile -File "$PS_PLUGIN/scripts/ensure-pses.ps1" 2>&1 | sed 's/^/    | /'
  if grep -q 'Analyzer' "$PS_PLUGIN/scripts/ensure-pses.ps1"; then
    echo "  ensure-pses.ps1 contains an analyzer check (see its output above)"
  else
    echo "  ensure-pses.ps1 in the mounted plugin has no analyzer check; testing PSScriptAnalyzer directly"
  fi
  CACHE="${CLAUDE_PSES_PATH:-${XDG_CACHE_HOME:-$HOME/.cache}/claude-powershell-lsp}"
  PSA="$(find "$CACHE" -name PSScriptAnalyzer.psd1 2>/dev/null | head -1)"
  if [ -z "$PSA" ]; then
    echo "  PSScriptAnalyzer.psd1 not found under $CACHE"
  else
    echo "  module: $PSA"
    printf 'function Frobnicate-Thing {\n    param([string]$Name)\n    Write-Output $Name\n}\n' >"$WORK/unapproved-verb.ps1"
    timeout 120 pwsh -NoLogo -NoProfile -Command "
      \$ErrorActionPreference = 'Continue'
      try { Import-Module '$PSA' -ErrorAction Stop; 'Import-Module: ok, PSScriptAnalyzer ' + (Get-Module PSScriptAnalyzer).Version }
      catch { 'Import-Module: FAILED: ' + \$_.Exception.Message; return }
      Invoke-ScriptAnalyzer -Path '$WORK/unapproved-verb.ps1' | Format-List RuleName, Severity, Line, Message | Out-String
    " 2>&1 | sed 's/^/    | /'
  fi
fi

# ---------------------------------------------------------------- g. embeddings
echo "== g. embeddings through Ollama"
if ! curl -fsS -m 5 "$OLLAMA_URL/api/tags" >"$LOG/g-tags.json" 2>"$LOG/g-curl.err"; then
  skip "g embeddings" "no Ollama answering at $OLLAMA_URL"
elif ! jq -e '[.models[].name] | any(startswith("embeddinggemma"))' "$LOG/g-tags.json" >/dev/null 2>&1; then
  skip "g embeddings" "Ollama at $OLLAMA_URL has no embeddinggemma model"
else
  GCFG="$WORK/config-embed.json"
  jq --arg u "$OLLAMA_URL" '.embed = {enabled: true, provider: "ollama", model: "embeddinggemma", base_url: $u}' "$CFG" >"$GCFG"
  GR="$WORK/embed-sh"
  mkrepo fixture-sh "$GR"
  cd "$GR" || exit 2
  CODE_INDEX_CONFIG="$GCFG" code-index index --embed >"$LOG/g-index.log" 2>&1 && pass "g1 index --embed" || { fail "g1 index --embed"; evidence "$LOG/g-index.log" 20; }
  CODE_INDEX_CONFIG="$GCFG" code-index search "upload the build artifact to an environment" >"$LOG/g-search.log" 2>&1
  if contains "$LOG/g-search.log" 'route: *vector'; then pass "g2 plain-language query reports route: vector"; else fail "g2 plain-language query reports route: vector"; evidence "$LOG/g-search.log" 12; fi
  evidence "$LOG/g-search.log" 6
fi
cd "$REPO" || exit 2

# ---------------------------------------------------------------- h. model in the loop
echo "== h. claude -p with the installed plugin"
if [ -z "${ANTHROPIC_API_KEY:-}" ] && [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
  skip "h model-in-the-loop" "no credentials in the container"
else
  cd "$SH" || exit 2
  # Re-index with a fresh DB so the model finds an index to search.
  code-index index --force >/dev/null 2>&1
  timeout 300 claude -p "Call the code-index search tool (mcp__code-index-search__search) with the query deploy_release, then reply with the file path of the hit." \
    --output-format stream-json --verbose \
    --allowedTools "mcp__code-index-search__search,mcp__code-index-search__show" \
    >"$LOG/h-stream.jsonl" 2>"$LOG/h-stream.err"
  if jq -e 'select(.type == "system" and .subtype == "init") | .tools | (index("mcp__code-index-search__search") != null and index("mcp__code-index-search__show") != null)' "$LOG/h-stream.jsonl" >/dev/null 2>&1; then
    pass "h1 init event lists the search and show tools"
  else fail "h1 init event lists the search and show tools"; evidence "$LOG/h-stream.err" 10; fi
  if jq -e 'select(.type == "user") | .message.content[]? | select(.type == "tool_result") | (.content | tostring) | select(test("deploy\\.sh"))' "$LOG/h-stream.jsonl" >/dev/null 2>&1; then
    pass "h2 a tool_result contains deploy.sh"
  else fail "h2 a tool_result contains deploy.sh"; tail -c 1500 "$LOG/h-stream.jsonl" | sed 's/^/      | /'; fi
  timeout 120 claude -p "/code" >"$LOG/h-code.log" 2>&1
  if grep -Eqi 'symbols|files|chunks' "$LOG/h-code.log"; then pass "h3 claude -p \"/code\" prints stats"; else fail "h3 claude -p \"/code\" prints stats"; fi
  evidence "$LOG/h-code.log" 8
  cd "$REPO" || exit 2
fi

echo
echo "== summary: $PASSES passed, $FAILS failed, $SKIPS skipped (logs in $LOG)"
[ "$FAILS" -eq 0 ]
