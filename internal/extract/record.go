// Package extract defines the contract between code-index and its extractors:
// the JSON-lines record schema (version 2), the Extractor interface the
// indexer drives, and the cache fingerprint. See extractors/README.md.
package extract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// SchemaVersion is the version of the record schema in this package. It is
// part of the cache fingerprint: raising it makes every language re-index.
const SchemaVersion = 2

// Record types, the value of Record.T.
const (
	TypeSymbol     = "symbol"      // a definition of any kind; see Record.Kind
	TypeFunc       = "func"        // shorthand for a symbol of kind "function"
	TypeAlias      = "alias"       // an alias definition
	TypeCall       = "call"        // a call site
	TypeDynamic    = "dynamic"     // a call whose target is only known at run time
	TypeDotSource  = "dotsource"   // the file's definitions are loaded into the caller's scope
	TypeImport     = "import"      // a module import (PowerShell)
	TypeInvoke     = "invoke"      // another workspace file executed as a child process
	TypeParseError = "parse_error" // the file has syntax errors; informational
	TypeError      = "error"       // the extractor failed on the file
)

// Record is one JSON line of extractor output. Line, Col, EndLine, EndCol,
// SelLine, SelCol, ToLine and ToCol are 1-based; columns count UTF-16 code
// units. File and every path-valued key (Path, To) are absolute. Keys that
// do not apply to a record type are absent. Which keys each type reads, and
// which are required, is the table in extractors/README.md.
type Record struct {
	T       string `json:"t"`
	File    string `json:"file,omitempty"`
	Name    string `json:"name,omitempty"`
	Alias   string `json:"alias,omitempty"`
	Target  string `json:"target,omitempty"`
	Path    string `json:"path,omitempty"`
	Expr    string `json:"expr,omitempty"`
	In      string `json:"in,omitempty"`
	Message string `json:"message,omitempty"`
	First   string `json:"first,omitempty"`
	Line    int    `json:"line,omitempty"`
	Col     int    `json:"col,omitempty"`
	EndLine int    `json:"endLine,omitempty"`
	EndCol  int    `json:"endCol,omitempty"`
	Count   int    `json:"count,omitempty"`

	// To is the file that holds the definition a call goes to; ToLine and
	// ToCol, when set, are the position of that definition's name.
	To         string `json:"to,omitempty"`
	ToLine     int    `json:"toLine,omitempty"`
	ToCol      int    `json:"toCol,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Reason     string `json:"reason,omitempty"`

	// Symbol and func records.
	Kind      string `json:"kind,omitempty"`    // lower-case LSP SymbolKind name
	SelLine   int    `json:"selLine,omitempty"` // position of the name
	SelCol    int    `json:"selCol,omitempty"`
	Qualified string `json:"qualified,omitempty"` // qualified name
	Signature string `json:"signature,omitempty"`
	Doc       string `json:"doc,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Parent is the zero-based ordinal of the parent among the same file's
	// symbol and func records in output order; nil means top level. Zero is a
	// valid ordinal, hence the pointer.
	Parent *int `json:"parent,omitempty"`
}

// IsSymbol reports whether r is a symbol or func record.
func (r Record) IsSymbol() bool { return r.T == TypeSymbol || r.T == TypeFunc }

// ParseLines decodes extractor output. A line is a record when it is a JSON
// object with a non-empty "t". Other lines (a warning printed to stdout, a
// line whose known key has the wrong JSON type) are counted in skipped and
// ignored. Unknown keys are ignored. Lines up to 8 MiB are read.
func ParseLines(r io.Reader) (recs []Record, skipped int, err error) {
	recs, skipped, _, err = parseLines(r)
	return recs, skipped, err
}

// parseLines is ParseLines that also returns the first skipped line (trimmed).
func parseLines(r io.Reader) (recs []Record, skipped int, firstSkipped string, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if line[0] != '{' || json.Unmarshal(line, &rec) != nil || rec.T == "" {
			if skipped == 0 {
				firstSkipped = string(line)
			}
			skipped++
			continue
		}
		recs = append(recs, rec)
	}
	return recs, skipped, firstSkipped, sc.Err()
}
