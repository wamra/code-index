# Deliberately broken syntax: used to check that a parse error does not stop indexing.
function Broken-Thing {
    param([string]$Name
    Write-Output "unterminated
