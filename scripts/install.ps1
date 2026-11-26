# Install code-index from a local checkout on Windows (PowerShell).
# Builds the binary with Go, puts it in $env:LOCALAPPDATA\code-index\bin and
# writes a starter config when none exists. Nothing under ~\.claude is touched
# (the code-search skill, the code-index-search plugin registration) unless you
# say so: pass -Skill and/or -Mod, pass -Yes, or answer the prompts. An existing,
# different skill file or an existing marketplace registration is left alone
# unless -Force is given. -DryRun prints what would change and changes nothing.
# -NoSkill and -NoMod skip those steps without asking.
# UNTESTED on Windows: written and syntax-checked on macOS only.
param(
  [string]$BinDir = (Join-Path $env:LOCALAPPDATA 'code-index\bin'),
  [switch]$Skill, [switch]$NoSkill, [switch]$Mod, [switch]$NoMod,
  [switch]$Yes, [switch]$Force, [switch]$DryRun
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $BinDir 'code-index.exe'
$config = $env:CODE_INDEX_CONFIG
if (-not $config) { $config = Join-Path $env:USERPROFILE '.config\code-index\config.json' }
$skillFile = Join-Path $env:USERPROFILE '.claude\skills\code-search\SKILL.md'

function Confirm-Step([string]$what, [bool]$explicit, [bool]$skip) {
  if ($skip) { return $false }
  if ($explicit -or $Yes) { return $true }
  if ([Environment]::UserInteractive -and -not [Console]::IsInputRedirected) {
    return ((Read-Host "$what [y/N]") -match '^(y|yes)$')
  }
  Write-Host "skipped: $what (not interactive; pass -Yes or the explicit switch to do it)"
  return $false
}

Write-Host 'This installer will change:'
Write-Host "  - $exe (the binary)"
if (Test-Path $config) { Write-Host "  - $config exists and is left as it is" } else { Write-Host "  - $config (a new starter config)" }
if (-not $NoSkill) { Write-Host "  - $skillFile (the code-search skill, under ~\.claude; asks first unless -Skill or -Yes)" }
if (-not $NoMod) { Write-Host '  - the Claude Code marketplace and plugin registration for code-index-search (under ~\.claude; asks first unless -Mod or -Yes)' }
if ($DryRun) { Write-Host 'dry run: nothing was changed.'; return }

if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Go is required to build from source: https://go.dev/dl/' }
New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
$version = 'dev'
$described = git -C $root describe --tags --always --dirty 2>$null
if ($LASTEXITCODE -eq 0 -and $described) { $version = $described }
$env:CGO_ENABLED = '0'
Push-Location $root
try { go build -ldflags "-s -w -X main.version=$version" -o $exe ./cmd/code-index } finally { Pop-Location }
& $exe version
if ($env:Path -notlike "*$BinDir*") { Write-Host "NOTE: add $BinDir to your PATH." }
if (-not (Test-Path $config)) { & $exe config init }
if (Confirm-Step 'Install the code-search skill into ~\.claude\skills?' $Skill.IsPresent $NoSkill.IsPresent) {
  if ($Force) { & $exe install-skill --force } else { & $exe install-skill; if ($LASTEXITCODE -ne 0) { Write-Host 'NOTE: the existing skill was kept (use -Force to replace it).' } }
}
if (Confirm-Step 'Register the code-index-search plugin with Claude Code (changes ~\.claude)?' $Mod.IsPresent $NoMod.IsPresent) {
  if (Get-Command claude -ErrorAction SilentlyContinue) {
    $registered = (-not $Force) -and ((claude plugin marketplace list 2>$null | Out-String) -match 'code-index')
    if ($registered) {
      Write-Host 'NOTE: a code-index marketplace is already registered; left as it is (use -Force to refresh it).'
    } else {
      claude plugin marketplace add $root
      if ($LASTEXITCODE -ne 0) { claude plugin marketplace update code-index }
      claude plugin install code-index-search@code-index --config "binary=$exe"
      Write-Host 'Restart Claude Code to load the search and show tools and /code.'
    }
  } else {
    Write-Host 'NOTE: claude is not on PATH; skipped the code-index-search mod.'
  }
}
& $exe doctor
Write-Host 'Next: cd <repo>; code-index index'
