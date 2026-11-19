# Install code-index from a local checkout on Windows (PowerShell).
# Builds the binary with Go, puts it in $env:LOCALAPPDATA\code-index\bin,
# writes a starter config when none exists, installs the code-search skill,
# then runs doctor.
# UNTESTED on Windows: written and cross-compile-checked on macOS only.
param([string]$BinDir = (Join-Path $env:LOCALAPPDATA 'code-index\bin'), [switch]$NoSkill)
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
& $exe doctor
Write-Host 'Next: cd <repo>; code-index index'
