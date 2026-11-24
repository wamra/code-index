// Package extractors embeds the AST extractor scripts that code-index runs
// next to the language servers.
package extractors

import _ "embed"

// PowerShellAST is powershell-ast.ps1.
//
//go:embed powershell-ast.ps1
var PowerShellAST []byte

// Embedded returns the contents of the embedded extractor file called name
// (the base name of the file in this directory, "powershell-ast.ps1").
func Embedded(name string) ([]byte, bool) {
	switch name {
	case "powershell-ast.ps1":
		return PowerShellAST, true
	}
	return nil, false
}
