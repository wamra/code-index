package extract

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/wamra/code-index/internal/lsp"
)

// The helpers in this file are the symbol-shaping steps the indexer applies to
// a documentSymbol answer (flatten, normalizeNames, locateName and their
// helpers in internal/indexer/symbols.go). internal/extract cannot import the
// indexer, so they are kept here with the same behaviour; a change in one
// must be made in the other until the merge wave moves them to a shared place.

// lspSym is a symbol with its parent resolved and its qualified name built.
type lspSym struct {
	lsp.Symbol
	Parent    int // index into the slice, -1 for top level
	Qualified string
}

// normalizeText applies the text normalisation a language server gets: invalid
// UTF-8 replaced, a leading BOM removed.
func normalizeText(data []byte) string {
	text := strings.ToValidUTF8(string(data), "\ufffd")
	return strings.TrimPrefix(text, "\ufeff")
}

// splitSourceLines splits text into lines without a trailing "\r", so LF and
// CRLF sources give the same lines.
func splitSourceLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// flattenSymbols turns a documentSymbol answer (hierarchical or flat) into a
// parent-before-child slice.
func flattenSymbols(syms []lsp.Symbol) []lspSym {
	var out []lspSym
	var rec func(list []lsp.Symbol, parent int, prefix string)
	rec = func(list []lsp.Symbol, parent int, prefix string) {
		for _, s := range list {
			q := s.Name
			if prefix != "" {
				q = prefix + "." + s.Name
			}
			out = append(out, lspSym{Symbol: s, Parent: parent, Qualified: q})
			idx := len(out) - 1
			out[idx].Children = nil
			rec(s.Children, idx, q)
		}
	}
	hier := false
	for _, s := range syms {
		if len(s.Children) > 0 {
			hier = true
		}
	}
	if hier {
		rec(syms, -1, "")
		return out
	}
	// Flat list: order by position so parents precede children, then find the
	// innermost enclosing symbol by range containment.
	list := append([]lsp.Symbol(nil), syms...)
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i].Range.Start, list[j].Range.Start
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Character != b.Character {
			return a.Character < b.Character
		}
		return list[i].Range.End.Line > list[j].Range.End.Line // larger range first
	})
	for i, s := range list {
		parent := -1
		for j := i - 1; j >= 0; j-- {
			if list[j].Range.ContainsRange(s.Range) && list[j].Range != s.Range {
				parent = j
				break
			}
		}
		q := s.Name
		switch {
		case parent >= 0:
			q = out[parent].Qualified + "." + s.Name
		case s.ContainerName != "":
			q = s.ContainerName + "." + s.Name
		}
		out = append(out, lspSym{Symbol: s, Parent: parent, Qualified: q})
	}
	return out
}

// normalizeNames replaces display-label names with the identifier the server
// selected ("function Get-Widget ()" becomes "Get-Widget", the label is kept as
// detail). Only labels that contain whitespace, whose single-line selection
// text does not, are touched.
func normalizeNames(syms []lsp.Symbol, lines []string) {
	for i := range syms {
		s := &syms[i]
		if s.SelectionRange != s.Range && strings.ContainsAny(s.Name, " \t") {
			if t, ok := rangeText(lines, s.SelectionRange); ok && t != "" && !strings.ContainsAny(t, " \t") && t != s.Name {
				if s.Detail == "" {
					s.Detail = s.Name
				}
				s.Name = t
			}
		}
		normalizeNames(s.Children, lines)
	}
}

// rangeText returns the text of a single-line range.
func rangeText(lines []string, r lsp.Range) (string, bool) {
	if r.Start.Line != r.End.Line || r.Start.Line < 0 || r.Start.Line >= len(lines) {
		return "", false
	}
	line := lines[r.Start.Line]
	a, b := byteIndexOfUTF16(line, r.Start.Character), byteIndexOfUTF16(line, r.End.Character)
	if a > b || b > len(line) {
		return "", false
	}
	return strings.TrimSpace(line[a:b]), true
}

// locateName fixes SelectionRange for symbols whose server gave only the full
// range (SymbolInformation): it finds the name in the first line of the range.
func locateName(s *lspSym, lines []string) {
	if s.SelectionRange != s.Range || s.Range.Start.Line >= len(lines) {
		return
	}
	line := lines[s.Range.Start.Line]
	from := 0
	if c := s.Range.Start.Character; c > 0 {
		from = byteIndexOfUTF16(line, c)
	}
	i := indexWord(line[from:], s.Name)
	if i < 0 {
		return
	}
	i += from
	start := len(utf16.Encode([]rune(line[:i])))
	s.SelectionRange.Start.Line = s.Range.Start.Line
	s.SelectionRange.Start.Character = start
	s.SelectionRange.End.Line = s.Range.Start.Line
	s.SelectionRange.End.Character = start + len(utf16.Encode([]rune(s.Name)))
}

// indexWord returns the byte index of the first occurrence of name in line
// that is not part of a longer identifier, else of the first occurrence at all,
// else -1.
func indexWord(line, name string) int {
	ident := func(r rune) bool { return r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	for off := 0; name != "" && off <= len(line)-len(name); {
		i := strings.Index(line[off:], name)
		if i < 0 {
			break
		}
		i += off
		before, _ := utf8.DecodeLastRuneInString(line[:i])
		after, _ := utf8.DecodeRuneInString(line[i+len(name):])
		if (i == 0 || !ident(before)) && (i+len(name) == len(line) || !ident(after)) {
			return i
		}
		off = i + 1
	}
	return strings.Index(line, name)
}

// byteIndexOfUTF16 returns the byte index in line of the UTF-16 column col.
func byteIndexOfUTF16(line string, col int) int {
	n := 0
	for i, r := range line {
		if n >= col {
			return i
		}
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return len(line)
}

// hoverKinds are the symbol kinds the indexer asks hover for.
var hoverKinds = map[string]bool{
	"function": true, "method": true, "constructor": true, "class": true, "interface": true,
	"struct": true, "enum": true, "module": true, "namespace": true, "package": true,
	"property": true, "event": true, "operator": true, "typeparameter": true,
}

// callableKinds are the kinds Edges asks call hierarchy or references for.
var callableKinds = map[string]bool{"function": true, "method": true, "constructor": true}
