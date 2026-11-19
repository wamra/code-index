. (Join-Path $PSScriptRoot 'lib.ps1')

$first = Get-Widget -Name 'alpha'
$second = gw -Name 'beta'
Invoke-Helper -Name 'gamma'

# dynamic call: the target is only known at run time
$cmd = 'Get-Widget'
& $cmd -Name 'delta'
