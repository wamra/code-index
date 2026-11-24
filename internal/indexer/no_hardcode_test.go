package indexer

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// No language or extension constant may live in the indexer or the extract
// package: PowerShell is a config entry (internal/config's built-in table).
// This scans the identifiers and literals (not the comments) of the non-test
// Go files of both packages.
func TestNoPowerShellSpecificCode(t *testing.T) {
	banned := []string{"pwsh", "powershell", ".ps1", ".psm1", ".psd1"}
	for _, dir := range []string{".", "../extract"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no sources in %s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			file := fset.AddFile(f, fset.Base(), len(src))
			var sc scanner.Scanner
			sc.Init(file, src, nil, 0) // mode 0: comments are skipped
			for {
				pos, tok, lit := sc.Scan()
				if tok == token.EOF {
					break
				}
				if tok != token.IDENT && tok != token.STRING && tok != token.CHAR {
					continue
				}
				for _, b := range banned {
					if strings.Contains(strings.ToLower(lit), b) {
						t.Errorf("%s: %q mentions %q; language knowledge belongs in the built-in config table", fset.Position(pos), lit, b)
					}
				}
			}
		}
	}
}
