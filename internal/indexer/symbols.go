package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/wamra/code-index/internal/lsp"
)

// flatSym is a symbol with its parent resolved and qualified name built.
type flatSym struct {
	lsp.Symbol
	Parent    int // index into the slice, -1 for top level
	Qualified string
	Signature string
	Doc       string
	BodySHA   string
}

// flatten turns a documentSymbol response (hierarchical or flat) into a
// parent-before-child ordered slice.
func flatten(syms []lsp.Symbol) []flatSym {
	var out []flatSym
	var rec func(list []lsp.Symbol, parent int, prefix string)
	rec = func(list []lsp.Symbol, parent int, prefix string) {
		for _, s := range list {
			q := s.Name
			if prefix != "" {
				q = prefix + "." + s.Name
			}
			out = append(out, flatSym{Symbol: s, Parent: parent, Qualified: q})
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
		// larger range first
		return list[i].Range.End.Line > list[j].Range.End.Line
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
		out = append(out, flatSym{Symbol: s, Parent: parent, Qualified: q})
	}
	return out
}

// utf16Col converts a rune-offset-free byte index within line to a UTF-16 column.
func utf16Col(line string, byteIdx int) int {
	return len(utf16.Encode([]rune(line[:byteIdx])))
}

// locateName fixes SelectionRange for symbols whose server gave only the full
// range (SymbolInformation): it finds the name in the first line of the range.
func locateName(s *flatSym, lines []string) {
	if s.SelectionRange != s.Range || s.Range.Start.Line >= len(lines) {
		return
	}
	line := lines[s.Range.Start.Line]
	from := 0
	if c := s.Range.Start.Character; c > 0 {
		from = byteIndexOfUTF16(line, c)
	}
	i := strings.Index(line[from:], s.Name)
	if i < 0 {
		return
	}
	i += from
	start := utf16Col(line, i)
	s.SelectionRange.Start.Line = s.Range.Start.Line
	s.SelectionRange.Start.Character = start
	s.SelectionRange.End.Line = s.Range.Start.Line
	s.SelectionRange.End.Character = start + len(utf16.Encode([]rune(s.Name)))
}

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

// bodySHA hashes the lines the symbol spans.
func bodySHA(lines []string, startLine, endLine int) string {
	if startLine < 0 {
		startLine = 0
	}
	if endLine >= len(lines) {
		endLine = len(lines) - 1
	}
	if startLine > endLine {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(lines[startLine:endLine+1], "\n")))
	return hex.EncodeToString(sum[:])
}

// hoverKinds are the symbol kinds worth a hover request (and a chunk).
var hoverKinds = map[string]bool{
	"function": true, "method": true, "constructor": true, "class": true, "interface": true,
	"struct": true, "enum": true, "module": true, "namespace": true, "package": true,
	"property": true, "event": true, "operator": true, "typeparameter": true,
}

// chunkSkipKinds are data-like kinds that get no chunk (they stay searchable
// by name through name_fts).
var chunkSkipKinds = map[string]bool{
	"variable": true, "constant": true, "string": true, "number": true, "boolean": true,
	"array": true, "object": true, "key": true, "null": true, "unknown": true,
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

// SymbolChunk is the text embedded and full-text indexed for one symbol:
//
//	<kind> <qualified_name>
//	file: <relative path>
//	signature: <signature>
//	doc: <doc comment>
//
// (summary, calls and called-by lines arrive with later milestones).
func SymbolChunk(kind, qualified, path, signature, doc string) string {
	var b strings.Builder
	b.WriteString(kind + " " + qualified + "\n")
	b.WriteString("file: " + path + "\n")
	if signature != "" {
		b.WriteString("signature: " + oneLine(signature, 500) + "\n")
	}
	if doc != "" {
		b.WriteString("doc: " + oneLine(doc, 1500) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// FileChunk is the text for the per-file chunk: path, language and the
// contained symbols.
func FileChunk(path, lang string, syms []flatSym) string {
	var b strings.Builder
	b.WriteString("file " + path + "\n")
	b.WriteString("lang: " + lang + "\n")
	var names []string
	for _, s := range syms {
		if chunkSkipKinds[s.Kind] {
			continue
		}
		names = append(names, s.Qualified+" ("+s.Kind+")")
		if len(names) == 200 {
			break
		}
	}
	if len(names) > 0 {
		b.WriteString("symbols: " + strings.Join(names, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// leadingComment returns the comment lines directly above startLine (zero-based),
// with the comment marker removed. It is the documentation fallback for servers
// whose hover has nothing at a definition.
func leadingComment(lines []string, startLine int) string {
	var out []string
	for i := startLine - 1; i >= 0 && i < len(lines); i-- {
		t := strings.TrimSpace(lines[i])
		var body string
		switch {
		case strings.HasPrefix(t, "#!"):
			i = -1
			continue
		case strings.HasPrefix(t, "///"):
			body = t[3:]
		case strings.HasPrefix(t, "//"):
			body = t[2:]
		case strings.HasPrefix(t, "#"):
			body = strings.TrimLeft(t, "#")
		case strings.HasPrefix(t, "--"):
			body = t[2:]
		default:
			i = -1
			continue
		}
		out = append([]string{strings.TrimSpace(body)}, out...)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// declarationLine is the first line of the symbol's range, trimmed and without
// a trailing block opener: the signature fallback when hover gives none.
func declarationLine(lines []string, startLine int) string {
	if startLine < 0 || startLine >= len(lines) {
		return ""
	}
	t := strings.TrimSpace(lines[startLine])
	t = strings.TrimSpace(strings.TrimSuffix(t, "{"))
	return oneLine(t, 200)
}

// normalizeNames replaces display-label names with the identifier the server
// selected. PowerShell Editor Services, for one, reports a function as
// "function Get-Widget ()" and puts the real name in selectionRange. The rule
// is deliberately narrow: only labels that contain whitespace, whose
// single-line selection text does not. The original label is kept as detail
// (it becomes the signature fallback).
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

// helpComment returns a PowerShell comment-based-help block (<# ... #>) that
// opens within the first lines of the symbol's body, without the .KEYWORD
// lines.
func helpComment(lines []string, startLine, endLine int) string {
	if endLine >= len(lines) {
		endLine = len(lines) - 1
	}
	first := -1
	for i := startLine; i <= endLine && i <= startLine+3; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "<#") {
			first = i
			break
		}
	}
	if first < 0 {
		return ""
	}
	var out []string
	for i := first; i <= endLine; i++ {
		t := strings.TrimSpace(lines[i])
		done := strings.Contains(t, "#>")
		t = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.Split(t, "#>")[0], "<#"), "\r"))
		if t != "" && !(strings.HasPrefix(t, ".") && len(strings.Fields(t)) == 1) {
			out = append(out, t)
		}
		if done {
			break
		}
	}
	return strings.Join(out, "\n")
}
