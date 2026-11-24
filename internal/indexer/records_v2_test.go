package indexer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/indexer"
)

// recordsExtract serves the given records (paths relative to root in file, to
// and path; made absolute here) as the output of the PowerShell AST extractor.
func recordsExtract(t *testing.T, root string, recs ...map[string]any) indexer.ExtractFunc {
	return func(context.Context, string, []string) ([]byte, error) {
		var out bytes.Buffer
		for _, m := range recs {
			c := map[string]any{}
			for k, v := range m {
				if s, ok := v.(string); ok && (k == "file" || k == "to" || k == "path") && s != "" {
					v = filepath.Join(root, filepath.FromSlash(s))
				}
				c[k] = v
			}
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			out.Write(b)
			out.WriteByte('\n')
		}
		return out.Bytes(), nil
	}
}

type rec = map[string]any

const (
	dupA = "function Dup {\n}\nfunction Caller {\n  Dup\n  Dup\n  Dup\n  Dup\n  Dup\n}\n"
	dupB = "function Dup {\n}\nfunction Other {\n}\n"
)

func TestCallRecordsV2(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": dupA, "b.ps1": dupB, "c.ps1": "function Third {\n}\n"})
	st := openStore(t)
	cfg := astCfg("")
	caller := func(line int, extra rec) rec {
		r := rec{"t": "call", "file": "a.ps1", "line": line, "col": 3, "in": "Caller", "name": "Dup"}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	ex := recordsExtract(t, root,
		// line 4: "to" b.ps1 -> the Dup in b.ps1, although a.ps1 has one too (name-only would pick a.ps1's)
		caller(4, rec{"to": "b.ps1", "resolution": "resolved", "reason": "direct"}),
		// line 5: position of the name in b.ps1 ("function Dup" at 1:10) picks the symbol
		caller(5, rec{"to": "b.ps1", "toLine": 1, "toCol": 10, "resolution": "candidate", "reason": "string-eval"}),
		// line 6: resolved + reason alias is an alias edge; dst_name stays the record's name
		caller(6, rec{"to": "b.ps1", "resolution": "resolved", "reason": "alias"}),
		// line 7: "to" is a file the language does not index; line 8 holds no such symbol
		caller(7, rec{"to": "missing.ps1", "resolution": "exact"}),
		caller(8, rec{"to": "c.ps1", "resolution": "exact"}),
		// no resolution at all: a schema v1 record
		rec{"t": "call", "file": "a.ps1", "line": 9, "col": 1, "name": "Dup"},
		rec{"t": "call", "file": "a.ps1", "line": 9, "col": 1, "name": "Dup", "resolution": "unresolved"},
		// name only, candidate: the lookup is today's, the confidence candidate
		rec{"t": "call", "file": "b.ps1", "line": 6, "col": 1, "name": "Other", "resolution": "candidate", "reason": "name-match"},
		// invoke of an indexed file; of one that is not indexed; unresolved
		rec{"t": "invoke", "file": "a.ps1", "line": 10, "col": 1, "path": "b.ps1", "resolution": "resolved", "reason": "direct"},
		rec{"t": "invoke", "file": "a.ps1", "line": 11, "col": 1, "path": "nope.ps1", "resolution": "resolved"},
		rec{"t": "invoke", "file": "a.ps1", "line": 11, "col": 1, "path": "c.ps1", "resolution": "unresolved"},
		// dotsource: unresolved gives no edge, no resolution resolves as before
		rec{"t": "dotsource", "file": "a.ps1", "line": 12, "col": 1, "path": "b.ps1", "resolution": "unresolved", "reason": "not-found"},
		rec{"t": "dotsource", "file": "c.ps1", "line": 5, "col": 1, "path": "b.ps1", "reason": "direct"},
	)
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: ex})
	notes := strings.Join(res.Notes, "\n")
	for _, want := range []string{
		"fake: ignored 2 call record(s) without a resolution (extractor powershell-ast writes schema v1)",
		"fake: 2 call record(s) point to a definition that is not indexed",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	rows, err := st.DB().Query(`SELECT e.kind, f.path, e.line+1, COALESCE(ss.qualified_name,'(file)'),
			COALESCE(ds.qualified_name, e.dst_name), COALESCE(dsf.path,''), e.source, e.confidence, COALESCE(e.reason,'')
		FROM edges e JOIN files f ON f.id=e.file_id LEFT JOIN symbols ss ON ss.id=e.src_symbol_id
		LEFT JOIN symbols ds ON ds.id=e.dst_symbol_id LEFT JOIN files dsf ON dsf.id=ds.file_id
		ORDER BY e.kind, f.path, e.line, e.col`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var kind, p, src, dst, dstFile, source, conf, reason string
		var line int
		if err := rows.Scan(&kind, &p, &line, &src, &dst, &dstFile, &source, &conf, &reason); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.TrimSpace(strings.Join([]string{kind, p + ":" + itoa(line), src, "->", dst, "in", dstFile, "[" + source, conf, reason + "]"}, " ")))
	}
	want := []string{
		"calls a.ps1:4 Caller -> Dup in b.ps1 [ast exact direct]",
		"calls a.ps1:5 Caller -> Dup in b.ps1 [ast candidate string-eval]",
		"calls a.ps1:6 Caller -> Dup in b.ps1 [ast alias alias]",
		"calls b.ps1:6 (file) -> Other in b.ps1 [ast candidate name-match]",
		"dot_sources c.ps1:5 (file) -> b.ps1 in  [ast exact direct]",
		"invokes a.ps1:10 (file) -> b.ps1 in  [ast exact direct]",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("edges:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// A record that names its target file is authoritative for its call site: the
// LSP edge to the other Dup (the fake server finds every Dup by word) is
// dropped, the LSP edge to the named one merges with the record and the row
// keeps the record's confidence and reason.
func TestToRecordOverridesLSPEdgesAtItsSite(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": "function Dup {\n}\nfunction Caller {\n  Dup\n}\n", "b.ps1": dupB})
	st := openStore(t)
	ex := recordsExtract(t, root, rec{"t": "call", "file": "a.ps1", "line": 4, "col": 3, "in": "Caller", "name": "Dup",
		"to": "b.ps1", "resolution": "candidate", "reason": "variable-constant"})
	runOpts(t, st, astCfg("refs"), indexer.Options{Root: root, Extract: ex})
	rows, err := st.DB().Query(`SELECT dsf.path, e.source, e.confidence, COALESCE(e.reason,'') FROM edges e
		JOIN symbols ds ON ds.id=e.dst_symbol_id JOIN files dsf ON dsf.id=ds.file_id JOIN files f ON f.id=e.file_id
		WHERE e.kind='calls' AND f.path='a.ps1' AND e.line=3`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var a, b, c, d string
		rows.Scan(&a, &b, &c, &d)
		got = append(got, strings.Join([]string{a, b, c, d}, " "))
	}
	if len(got) != 1 || got[0] != "b.ps1 lsp+ast candidate variable-constant" {
		t.Errorf("edges at a.ps1:4 = %q", got)
	}
}
