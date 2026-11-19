package indexer

import (
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/lsp"
)

const crlfSrc = "# Adds numbers.\r\n# Second line.\r\nfunction Add-Numbers {\r\n    <#\r\n    .SYNOPSIS\r\n    Sums two values.\r\n    #>\r\n    param($a, $b)\r\n}\r\n"

func TestSplitLinesIgnoresLineEndingStyle(t *testing.T) {
	lf := strings.ReplaceAll(crlfSrc, "\r\n", "\n")
	a, b := splitLines(crlfSrc), splitLines(lf)
	if len(a) != len(b) {
		t.Fatalf("%d vs %d lines", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] || strings.Contains(a[i], "\r") {
			t.Errorf("line %d: %q vs %q", i, a[i], b[i])
		}
	}
	if bodySHA(a, 2, 8) != bodySHA(b, 2, 8) {
		t.Error("body hash must not depend on CRLF vs LF")
	}
}

func TestDocExtractionWithCRLF(t *testing.T) {
	lines := splitLines(crlfSrc)
	if got := leadingComment(lines, 2); got != "Adds numbers.\nSecond line." {
		t.Errorf("leadingComment = %q", got)
	}
	if got := helpComment(lines, 2, 8); got != "Sums two values." {
		t.Errorf("helpComment = %q", got)
	}
	if got := declarationLine(lines, 2); got != "function Add-Numbers" {
		t.Errorf("declarationLine = %q", got)
	}
	// even without splitLines the helpers must not leak "\r"
	raw := strings.Split(crlfSrc, "\n")
	for name, got := range map[string]string{
		"leadingComment":  leadingComment(raw, 2),
		"helpComment":     helpComment(raw, 2, 8),
		"declarationLine": declarationLine(raw, 2),
	} {
		if strings.Contains(got, "\r") {
			t.Errorf("%s leaked a carriage return: %q", name, got)
		}
	}
}

func TestLocateNameAndRangeTextWithCRLFAndUTF16(t *testing.T) {
	// "é" is 1 UTF-16 unit, the emoji 2: columns are UTF-16 code units
	lines := splitLines("# é 😀\r\nfunction Get-Äpfel {\r\n}\r\n")
	s := flatSym{Symbol: lsp.Symbol{Name: "Get-Äpfel", Range: lsp.Range{Start: lsp.Position{Line: 1}, End: lsp.Position{Line: 2, Character: 1}}}}
	s.SelectionRange = s.Range
	locateName(&s, lines)
	if s.SelectionRange.Start.Character != 9 || s.SelectionRange.End.Character != 18 {
		t.Errorf("selection = %+v", s.SelectionRange)
	}
	if txt, ok := rangeText(lines, s.SelectionRange); !ok || txt != "Get-Äpfel" {
		t.Errorf("rangeText = %q %v", txt, ok)
	}
	emoji := splitLines("x = '😀'; function F {\r\n}")
	if got := byteIndexOfUTF16(emoji[0], 8); emoji[0][got:got+1] != ";" {
		t.Errorf("byteIndexOfUTF16 after an emoji points at %q", emoji[0][got:])
	}
}
