<#
.SYNOPSIS
Small widget helpers used by the fixture workspace.
#>

function Get-Widget {
    <#
    .SYNOPSIS
    Returns a widget by name.
    #>
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][string]$Name
    )
    [pscustomobject]@{ Name = $Name; Size = $Name.Length }
}

function New-Widget {
    <#
    .SYNOPSIS
    Creates a new widget with the given name and size.
    #>
    param([string]$Name, [int]$Size = 1)
    [pscustomobject]@{ Name = $Name; Size = $Size }
}

Set-Alias -Name gw -Value Get-Widget
Export-ModuleMember -Function Get-Widget, New-Widget -Alias gw
