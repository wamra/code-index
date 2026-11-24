package lsp

import (
	"encoding/json"
	"strings"
)

// Position is a zero-based line and UTF-16 column.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a span of text.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Contains reports whether r contains p.
func (r Range) Contains(p Position) bool {
	if p.Line < r.Start.Line || p.Line > r.End.Line {
		return false
	}
	if p.Line == r.Start.Line && p.Character < r.Start.Character {
		return false
	}
	if p.Line == r.End.Line && p.Character > r.End.Character {
		return false
	}
	return true
}

// ContainsRange reports whether o lies inside r.
func (r Range) ContainsRange(o Range) bool { return r.Contains(o.Start) && r.Contains(o.End) }

// SymbolKinds maps LSP SymbolKind numbers (1-26) to lowercase names.
var SymbolKinds = [...]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class", 6: "method",
	7: "property", 8: "field", 9: "constructor", 10: "enum", 11: "interface",
	12: "function", 13: "variable", 14: "constant", 15: "string", 16: "number",
	17: "boolean", 18: "array", 19: "object", 20: "key", 21: "null",
	22: "enummember", 23: "struct", 24: "event", 25: "operator", 26: "typeparameter",
}

// KindName returns the lowercase SymbolKind name ("unknown" out of range).
func KindName(k int) string {
	if k >= 1 && k < len(SymbolKinds) {
		return SymbolKinds[k]
	}
	return "unknown"
}

// Symbol is a normalized document symbol: hierarchical responses keep their
// Children, flat (SymbolInformation) responses set ContainerName.
type Symbol struct {
	Name           string
	Detail         string
	Kind           string
	Range          Range
	SelectionRange Range
	ContainerName  string
	Children       []Symbol
}

type rawSymbol struct {
	Name           string       `json:"name"`
	Detail         string       `json:"detail"`
	Kind           int          `json:"kind"`
	Range          *Range       `json:"range"`
	SelectionRange *Range       `json:"selectionRange"`
	Children       []rawSymbol  `json:"children"`
	Location       *rawLocation `json:"location"`
	ContainerName  string       `json:"containerName"`
}

type rawLocation struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

func (r rawSymbol) normalize() Symbol {
	s := Symbol{Name: r.Name, Detail: r.Detail, Kind: KindName(r.Kind), ContainerName: r.ContainerName}
	switch {
	case r.Range != nil: // DocumentSymbol
		s.Range = *r.Range
		s.SelectionRange = *r.Range
		if r.SelectionRange != nil {
			s.SelectionRange = *r.SelectionRange
		}
	case r.Location != nil: // SymbolInformation
		s.Range = r.Location.Range
		s.SelectionRange = r.Location.Range
	}
	for _, c := range r.Children {
		s.Children = append(s.Children, c.normalize())
	}
	return s
}

// ParseDocumentSymbols decodes a textDocument/documentSymbol result: null,
// DocumentSymbol[] (hierarchical) or SymbolInformation[] (flat).
func ParseDocumentSymbols(raw json.RawMessage) ([]Symbol, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var rs []rawSymbol
	if err := json.Unmarshal(raw, &rs); err != nil {
		return nil, err
	}
	out := make([]Symbol, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.normalize())
	}
	return out, nil
}

// HoverText flattens a textDocument/hover result (MarkupContent, MarkedString
// or MarkedString[]) into Markdown/plain text. Code-marked strings become
// fenced blocks so ParseHover can find the signature.
func HoverText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return ""
	}
	var parts []string
	var add func(c json.RawMessage)
	add = func(c json.RawMessage) {
		c = json.RawMessage(strings.TrimSpace(string(c)))
		if len(c) == 0 {
			return
		}
		switch c[0] {
		case '"':
			var s string
			if json.Unmarshal(c, &s) == nil {
				parts = append(parts, s)
			}
		case '[':
			var arr []json.RawMessage
			if json.Unmarshal(c, &arr) == nil {
				for _, a := range arr {
					add(a)
				}
			}
		case '{':
			var o struct {
				Kind     string `json:"kind"`
				Language string `json:"language"`
				Value    string `json:"value"`
			}
			if json.Unmarshal(c, &o) != nil {
				return
			}
			if o.Language != "" { // MarkedString {language, value}
				parts = append(parts, "```"+o.Language+"\n"+o.Value+"\n```")
			} else {
				parts = append(parts, o.Value)
			}
		}
	}
	add(h.Contents)
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// ParseHover splits hover text into a signature (the first fenced code block,
// else the first line) and the documentation (everything else).
func ParseHover(text string) (signature, doc string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	lines := strings.Split(text, "\n")
	var sig []string
	var rest []string
	inFence, seen := false, false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			if inFence {
				inFence = false
				seen = true
			} else if !seen {
				inFence = true
			} else {
				rest = append(rest, l)
			}
			continue
		}
		switch {
		case inFence:
			sig = append(sig, l)
		default:
			rest = append(rest, l)
		}
	}
	if len(sig) == 0 {
		sig, rest = nil, nil
		for i, l := range lines {
			if strings.TrimSpace(l) != "" {
				sig = []string{l}
				rest = lines[i+1:]
				break
			}
		}
	}
	signature = strings.TrimSpace(strings.Join(sig, "\n"))
	var kept []string
	for _, l := range rest {
		if strings.TrimSpace(l) == "---" { // markdown rule between hover sections
			continue
		}
		if strings.TrimSpace(l) == "" && (len(kept) == 0 || strings.TrimSpace(kept[len(kept)-1]) == "") {
			continue // no leading or doubled blank lines
		}
		kept = append(kept, l)
	}
	doc = strings.TrimSpace(strings.Join(kept, "\n"))
	return signature, doc
}

// CallHierarchyItem is a callable the server knows, as answered by
// textDocument/prepareCallHierarchy and carried inside incoming calls. Data is
// opaque server state that must be sent back unchanged in callHierarchy/incomingCalls.
type CallHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Detail         string          `json:"detail,omitempty"`
	URI            string          `json:"uri"`
	Range          Range           `json:"range"`
	SelectionRange Range           `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// IncomingCall is one caller of a CallHierarchyItem. FromRanges are the call
// sites, in the document of From (not of the callee).
type IncomingCall struct {
	From       CallHierarchyItem `json:"from"`
	FromRanges []Range           `json:"fromRanges"`
}
