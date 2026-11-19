# Dot-sourced by main.ps1.
Import-Module (Join-Path $PSScriptRoot 'Widgets/Widgets.psd1') -Force

function Invoke-Helper {
    <#
    .SYNOPSIS
    Looks up a widget and prints its name.
    #>
    param([string]$Name)
    $w = Get-Widget -Name $Name
    Write-Output $w.Name
}
