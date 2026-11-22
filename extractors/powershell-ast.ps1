# code-index PowerShell AST extractor.
#
# Runs under PowerShell 7 (pwsh) and Windows PowerShell 5.1 (powershell.exe).
# Keep this file ASCII-only and free of PowerShell 7-only syntax: 5.1 reads a
# BOM-less script as ANSI, and a test enforces both.
#
# Usage: pwsh -NoLogo -NoProfile -File powershell-ast.ps1 -ListFile <file>
# ListFile holds one absolute script path per line. For every file the script
# parses it with the PowerShell language parser and writes one JSON object per
# line to stdout:
#   func       function definition            name, line, col, endLine, endCol
#   alias      alias definition               alias, target, line, col, endLine, endCol
#   call       command invocation             name, in
#   dynamic    call whose target is only known at run time   expr, in
#   dotsource  . <path> statement              path (last string literal), expr, in
#   import     Import-Module statement         path (name or path literal), expr, in
#   parse_error                                count, first
# Every object has file, line and col (1-based). "in" is the enclosing function.
param([Parameter(Mandatory)][string]$ListFile)

$ErrorActionPreference = 'Stop'
$Utf8 = New-Object System.Text.UTF8Encoding($false)
# Raw UTF-8 bytes to stdout: independent of the console code page (5.1 defaults to OEM/ANSI).
$Stdout = [Console]::OpenStandardOutput()

function Emit([hashtable]$o) {
    $bytes = $Utf8.GetBytes((($o | ConvertTo-Json -Compress -Depth 3) + "`n"))
    $Stdout.Write($bytes, 0, $bytes.Length)
}

function Clip([string]$s, [int]$n = 160) {
    $s = ($s -replace '\s+', ' ').Trim()
    if ($s.Length -gt $n) { $s = $s.Substring(0, $n) }
    $s
}

function Enclosing($ast) {
    for ($n = $ast.Parent; $null -ne $n; $n = $n.Parent) {
        if ($n -is [System.Management.Automation.Language.FunctionDefinitionAst]) { return $n.Name }
    }
    ''
}

# The last string literal inside an expression (or the expression itself).
function LastString($ast) {
    if ($ast -is [System.Management.Automation.Language.StringConstantExpressionAst]) { return $ast.Value }
    $found = $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.StringConstantExpressionAst] }, $true)
    if ($found.Count -gt 0) { return $found[$found.Count - 1].Value }
    ''
}

# The value of a command argument: a string literal's value, else its source text.
function ArgText($ast) {
    if ($ast -is [System.Management.Automation.Language.StringConstantExpressionAst]) { return $ast.Value }
    $ast.Extent.Text
}

# Arguments of a command by parameter name prefix; positional ones fill $positional in order.
function CommandArgs($c, [string[]]$names) {
    $named = @{}
    $pos = @()
    $els = $c.CommandElements
    for ($i = 1; $i -lt $els.Count; $i++) {
        $e = $els[$i]
        if ($e -is [System.Management.Automation.Language.CommandParameterAst]) {
            $hit = $null
            foreach ($n in $names) { if ($n.StartsWith($e.ParameterName, [StringComparison]::OrdinalIgnoreCase)) { $hit = $n; break } }
            $val = $null
            if ($null -ne $e.Argument) { $val = $e.Argument }
            elseif ($i + 1 -lt $els.Count -and $els[$i + 1] -isnot [System.Management.Automation.Language.CommandParameterAst]) { $val = $els[$i + 1]; $i++ }
            if ($null -ne $hit -and $null -ne $val) { $named[$hit] = $val }
        } else {
            $pos += $e
        }
    }
    @{ Named = $named; Positional = $pos }
}

$AliasCmds = @('set-alias', 'new-alias', 'sal', 'nal')
$ImportCmds = @('import-module', 'ipmo')
$IexCmds = @('invoke-expression', 'iex')

foreach ($path in [System.IO.File]::ReadAllLines($ListFile, $Utf8)) {
    if ([string]::IsNullOrWhiteSpace($path)) { continue }
    try {
        $tokens = $null; $errors = $null
        # Read as UTF-8 and parse the text: ParseFile would decode a BOM-less file as ANSI on 5.1,
        # which shifts columns after any non-ASCII character.
        $text = [System.IO.File]::ReadAllText($path, $Utf8)
        $ast = [System.Management.Automation.Language.Parser]::ParseInput($text, $path, [ref]$tokens, [ref]$errors)
        if ($errors.Count -gt 0) {
            Emit @{ t = 'parse_error'; file = $path; line = 1; col = 1; count = $errors.Count; first = (Clip $errors[0].Message) }
        }

        foreach ($f in $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $true)) {
            Emit @{ t = 'func'; file = $path; name = $f.Name; line = $f.Extent.StartLineNumber; col = $f.Extent.StartColumnNumber
                    endLine = $f.Extent.EndLineNumber; endCol = $f.Extent.EndColumnNumber }
        }

        # [Alias('x')] on a function's param block defines the alias x for that function.
        foreach ($a in $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.AttributeAst] -and $n.TypeName.Name -eq 'Alias' }, $true)) {
            $fn = $a.Parent
            if ($fn -isnot [System.Management.Automation.Language.ParamBlockAst]) { continue }
            $owner = $fn.Parent.Parent
            if ($owner -isnot [System.Management.Automation.Language.FunctionDefinitionAst]) { continue }
            foreach ($p in $a.PositionalArguments) {
                if ($p -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
                    Emit @{ t = 'alias'; file = $path; alias = $p.Value; target = $owner.Name; line = $p.Extent.StartLineNumber; col = $p.Extent.StartColumnNumber
                            endLine = $p.Extent.EndLineNumber; endCol = $p.Extent.EndColumnNumber }
                }
            }
        }

        foreach ($c in $ast.FindAll({ param($n) $n -is [System.Management.Automation.Language.CommandAst] }, $true)) {
            if ($c.CommandElements.Count -eq 0) { continue }
            $first = $c.CommandElements[0]
            $name = $c.GetCommandName()
            $in = Enclosing $c
            $base = @{ file = $path; line = $first.Extent.StartLineNumber; col = $first.Extent.StartColumnNumber; in = $in }

            if ($c.InvocationOperator -eq 'Dot') {
                $lit = if ($null -ne $name) { $name } else { LastString $first }
                Emit ($base + @{ t = 'dotsource'; path = $lit; expr = (Clip $first.Extent.Text) })
                continue
            }
            if ($null -eq $name) {
                if ($first -is [System.Management.Automation.Language.ScriptBlockExpressionAst]) { continue }  # & { ... }: its commands are visited anyway
                Emit ($base + @{ t = 'dynamic'; expr = (Clip ($(if ($c.InvocationOperator -eq 'Ampersand') { '& ' } else { '' }) + $first.Extent.Text)) })
                continue
            }
            $lname = $name.ToLowerInvariant()
            if ($AliasCmds -contains $lname) {
                $ca = CommandArgs $c @('Name', 'Value')
                $nameAst = if ($ca.Named.ContainsKey('Name')) { $ca.Named['Name'] } elseif ($ca.Positional.Count -ge 1) { $ca.Positional[0] } else { $null }
                $valAst = if ($ca.Named.ContainsKey('Value')) { $ca.Named['Value'] } elseif ($ca.Positional.Count -ge 2) { $ca.Positional[1] } else { $null }
                if ($null -ne $nameAst -and $null -ne $valAst) {
                    Emit @{ t = 'alias'; file = $path; alias = (ArgText $nameAst); target = (ArgText $valAst); line = $nameAst.Extent.StartLineNumber; col = $nameAst.Extent.StartColumnNumber
                            endLine = $nameAst.Extent.EndLineNumber; endCol = $nameAst.Extent.EndColumnNumber; in = $in }
                }
                continue
            }
            if ($ImportCmds -contains $lname) {
                $ca = CommandArgs $c @('Name', 'Path', 'FullyQualifiedName')
                $arg = $null
                foreach ($k in 'Name', 'Path', 'FullyQualifiedName') { if ($null -eq $arg -and $ca.Named.ContainsKey($k)) { $arg = $ca.Named[$k] } }
                if ($null -eq $arg -and $ca.Positional.Count -ge 1) { $arg = $ca.Positional[0] }
                if ($null -ne $arg) { Emit ($base + @{ t = 'import'; path = (LastString $arg); expr = (Clip $arg.Extent.Text) }) }
                continue
            }
            if ($IexCmds -contains $lname) {
                $arg = if ($c.CommandElements.Count -gt 1) { $c.CommandElements[1].Extent.Text } else { '' }
                Emit ($base + @{ t = 'dynamic'; expr = (Clip "$name $arg") })
                continue
            }
            Emit ($base + @{ t = 'call'; name = $name })
        }
    } catch {
        Emit @{ t = 'error'; file = $path; line = 1; col = 1; message = (Clip $_.Exception.Message) }
    }
}
$Stdout.Flush()
