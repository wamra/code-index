package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/extract"
	"github.com/wamra/code-index/internal/store"
)

const bashSrc = "#!/bin/bash\n# Deploys the release.\n# Second line.\ndeploy() {\n  build\n}\n\nfunction f {\n  :\n}\n"

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// func records shaped like wammybash writes them (name and range only) become
// function symbols with the doc comment and declaration line as the fallbacks.
func TestRecordSymbolsFromWammybashShapedFuncRecords(t *testing.T) {
	recs := []extract.Record{
		{T: "func", File: "/w/a.sh", Name: "deploy", Line: 4, Col: 1, EndLine: 6, EndCol: 2},
		{T: "call", File: "/w/a.sh", Name: "build", Line: 5, Col: 3, Resolution: "resolved"}, // not a symbol: skipped, not counted
		{T: "func", File: "/w/a.sh", Name: "f", Line: 8, Col: 1, EndLine: 10, EndCol: 2},
	}
	syms := recordSymbols(recs, splitLines(bashSrc))
	if len(syms) != 2 {
		t.Fatalf("syms = %+v", syms)
	}
	d := syms[0]
	if d.Kind != "function" || d.Qualified != "deploy" || d.Parent != -1 {
		t.Errorf("deploy = %+v", d)
	}
	if d.Doc != "Deploys the release.\nSecond line." {
		t.Errorf("doc = %q", d.Doc)
	}
	if d.Signature != "deploy()" {
		t.Errorf("signature = %q", d.Signature)
	}
	if d.Range.Start.Line != 3 || d.Range.End.Line != 5 || d.BodySHA == "" {
		t.Errorf("range = %+v sha=%q", d.Range, d.BodySHA)
	}
	// the name is located in the first line of the range, not at the "f" of "function"
	if f := syms[1]; f.SelectionRange.Start.Line != 7 || f.SelectionRange.Start.Character != 9 || f.Doc != "" || f.Signature != "function f" {
		t.Errorf("f = %+v", f)
	}
}

func TestRecordSymbolsKindsParentsAndOverrides(t *testing.T) {
	zero, one := 0, 1
	src := "class A {\n  run() {\n  }\n}\n"
	recs := []extract.Record{
		{T: "symbol", Name: "A", Kind: "Class", Line: 1, Col: 1, EndLine: 4, EndCol: 2, SelLine: 1, SelCol: 7},
		{T: "symbol", Name: "run", Kind: "method", Line: 2, Col: 3, EndLine: 3, EndCol: 4, Parent: &zero, Signature: "run()", Doc: "runs"},
		{T: "symbol", Name: "", Kind: "method", Line: 3, Col: 1},                    // skipped, but it holds ordinal 2
		{T: "symbol", Name: "bad", Kind: "variable", Line: 2, Col: 1, Parent: &one}, // ordinal 1 is run: a child of run
		{T: "symbol", Name: "orphan", Kind: "variable", Line: 2, Col: 1, Parent: &one, Qualified: "x.orphan"},
		{T: "symbol", Name: "fwd", Kind: "variable", Line: 2, Col: 1, Parent: func() *int { n := 9; return &n }()}, // forward: top level
	}
	syms := recordSymbols(recs, splitLines(src))
	if len(syms) != 5 {
		t.Fatalf("syms = %+v", syms)
	}
	if a := syms[0]; a.Kind != "class" || a.SelectionRange.Start.Character != 6 || a.SelectionRange.End.Character != 7 {
		t.Errorf("A = %+v", a)
	}
	if r := syms[1]; r.Parent != 0 || r.Qualified != "A.run" || r.Signature != "run()" || r.Doc != "runs" {
		t.Errorf("run = %+v", r)
	}
	if b := syms[2]; b.Parent != 1 || b.Qualified != "A.run.bad" {
		t.Errorf("bad = %+v", b)
	}
	if o := syms[3]; o.Qualified != "x.orphan" {
		t.Errorf("orphan = %+v", o)
	}
	if f := syms[4]; f.Parent != -1 {
		t.Errorf("fwd = %+v", f)
	}
}

func TestIndexFromRecordsWritesSymbolsWithExtractSource(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	root := t.TempDir()
	c := candidate{rel: "a.sh", abs: filepath.Join(root, "a.sh"), lang: "bash", size: int64(len(bashSrc)), mtime: 1}
	recs := []extract.Record{
		{T: "func", File: c.abs, Name: "deploy", Line: 4, Col: 1, EndLine: 6, EndCol: 2},
		{T: "func", File: c.abs, Name: "f", Line: 8, Col: 1, EndLine: 10, EndCol: 2},
	}
	fs, err := indexFromRecords(ctx, st, c, []byte(bashSrc), sha256Hex([]byte(bashSrc)), 0, false, recs)
	if err != nil || fs.symbols != 2 || fs.chunks != 3 {
		t.Fatalf("stats = %+v err=%v", fs, err)
	}
	var kind, source, doc, sig string
	if err := st.DB().QueryRow(`SELECT kind, source, doc, signature FROM symbols WHERE name='deploy'`).Scan(&kind, &source, &doc, &sig); err != nil {
		t.Fatal(err)
	}
	if kind != "function" || source != "extract" || !strings.HasPrefix(doc, "Deploys the release.") || !strings.HasPrefix(sig, "deploy()") {
		t.Errorf("row = %q %q %q %q", kind, source, doc, sig)
	}
	// an edge rebuild deletes 'ast' symbols only: these survive it
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteLangEdges(ctx, tx, "bash", false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM symbols WHERE source='extract'`).Scan(&n); err != nil || n != 2 {
		t.Errorf("symbols after deleteLangEdges = %d (%v)", n, err)
	}

	// knownSymbols hands them back as records, 1-based, absolute, with parents as ordinals
	known, err := knownSymbols(ctx, st.DB(), root, "bash")
	if err != nil || len(known) != 2 {
		t.Fatalf("known = %+v err=%v", known, err)
	}
	if k := known[1]; k.T != "symbol" || k.File != c.abs || k.Name != "f" || k.Kind != "function" || k.Line != 8 || k.SelLine != 8 || k.SelCol != 10 || k.Parent != nil {
		t.Errorf("known[1] = %+v", k)
	}
}

func TestKnownSymbolsParentOrdinalsRestartPerFile(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	root := t.TempDir()
	zero := 0
	src := "class A {\n  run() {\n  }\n}\n"
	for _, name := range []string{"a.ps1", "b.ps1"} {
		c := candidate{rel: name, abs: filepath.Join(root, name), lang: "ps", size: 1, mtime: 1}
		recs := []extract.Record{
			{T: "symbol", Name: "A", Kind: "class", Line: 1, Col: 1, EndLine: 4, EndCol: 2},
			{T: "symbol", Name: "run", Kind: "method", Line: 2, Col: 3, EndLine: 3, EndCol: 4, Parent: &zero},
		}
		if _, err := indexFromRecords(ctx, st, c, []byte(src), sha256Hex([]byte(name)), 0, false, recs); err != nil {
			t.Fatal(err)
		}
	}
	known, err := knownSymbols(ctx, st.DB(), root, "ps")
	if err != nil || len(known) != 4 {
		t.Fatalf("known = %+v err=%v", known, err)
	}
	for i, k := range known {
		isChild := k.Name == "run"
		if isChild != (k.Parent != nil) || (isChild && *k.Parent != 0) {
			t.Errorf("known[%d] = %+v parent=%v", i, k, k.Parent)
		}
	}
}

func TestIndexWord(t *testing.T) {
	for _, c := range []struct {
		line, name string
		want       int
	}{
		{"function f {", "f", 9},
		{"deploy() {", "deploy", 0},
		{"function Get-Item-Two {", "Get-Item", 9}, // only inside a longer name: first occurrence
		{"x foo-bar foo", "foo", 10},
		{"nothing", "zzz", -1},
	} {
		if got := indexWord(c.line, c.name); got != c.want {
			t.Errorf("indexWord(%q, %q) = %d, want %d", c.line, c.name, got, c.want)
		}
	}
}
