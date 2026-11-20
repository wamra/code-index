// Package extractors embeds the AST extractor scripts that code-index runs
// next to the language servers.
package extractors

import _ "embed"

// PowerShellAST is powershell-ast.ps1.
//
//go:embed powershell-ast.ps1
var PowerShellAST []byte
