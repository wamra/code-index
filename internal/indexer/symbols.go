package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

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
	i := indexWord(line[from:], s.Name)
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

// indexWord returns the byte index of the first occurrence of name in line
// that is not part of a longer identifier ("f" in "function f" is the last
// letter, not the first), else of the first occurrence at all, else -1.
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

// splitLines splits source text into lines for position arithmetic and comment
// extraction. LF and CRLF files give the same lines (no trailing "\r"), so
// columns, hashes, doc comments and declaration lines do not depend on the
// line-ending style.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}
