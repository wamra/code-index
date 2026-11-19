# Install code-index from a local checkout on Windows (PowerShell).
# Builds the binary with Go, puts it in $env:LOCALAPPDATA\code-index\bin,
# writes a starter config when none exists, installs the code-search skill and
# the code-index-search mod (model tools and /code in Claude Code), then runs
# doctor.
# UNTESTED on Windows: written and cross-compile-checked on macOS only.
param([string]$BinDir = (Join-Path $env:LOCALAPPDATA 'code-index\bin'), [switch]$NoSkill, [switch]$NoMod)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go is required to build from source: https://go.dev/dl/' }
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
$exe = Join-Path $BinDir 'code-index.exe'
$version = 'dev'
$described = git -C $root describe --tags --always --dirty 2>$null
if ($LASTEXITCODE -eq 0 -and $described) { $version = $described }
$env:CGO_ENABLED = '0'
Push-Location $root
try { go build -ldflags "-s -w -X main.version=$version" -o $exe ./cmd/code-index } finally { Pop-Location }
& $exe version
if ($env:Path -notlike "*$BinDir*") { Write-Host "NOTE: add $BinDir to your PATH." }
$config = $env:CODE_INDEX_CONFIG
if (-not $config) { $config = Join-Path $env:USERPROFILE '.config\code-index\config.json' }
if (-not (Test-Path $config)) { & $exe config init }
if (-not $NoSkill) { & $exe install-skill }
if (-not $NoMod) {
  if (Get-Command claude -ErrorAction SilentlyContinue) {
    claude plugin marketplace add $root
    if ($LASTEXITCODE -ne 0) { claude plugin marketplace update code-index }
    claude plugin install code-index-search@code-index --config "binary=$exe"
    Write-Host 'Restart Claude Code to load the search and show tools and /code.'
  } else {
    Write-Host 'NOTE: claude is not on PATH; skipped the code-index-search mod.'
  }
}
& $exe doctor
Write-Host 'Next: cd <repo>; code-index index'
