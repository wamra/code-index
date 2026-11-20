package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/store"
)

// graphTree replaces the fixture with a small call graph:
// Alpha -> Beta -> Gamma -> Alpha (a cycle), Delta calls Gamma, Lone is unconnected,
// and two files define a function called Dup.
func graphTree(t *testing.T, e env) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.root, "*"))
	for _, f := range files {
		os.RemoveAll(f)
	}
	for p, body := range map[string]string{
		"cycle.ps1": "function Alpha {\n  Beta\n}\nfunction Beta {\n  Gamma\n}\nfunction Gamma {\n  Alpha\n}\nfunction Delta {\n  Gamma\n}\nfunction Lone {\n}\nAlpha\n",
		"d1.ps1":    "function Dup {\n}\n",
		"d2.ps1":    "function Dup {\n}\n",
	} {
		os.WriteFile(filepath.Join(e.root, p), []byte(body), 0o644)
	}
}

func TestCallGraphCommands(t *testing.T) {
	e := setupOpts(t, "", "refs")
	graphTree(t, e)
	out := mustOK(t, "index", e.root, "--quiet")
	if !strings.Contains(out, "edges: 5 written") {
		t.Fatalf("index summary: %s", out)
	}

	if got := mustOK(t, "callers", "Gamma"); got != "cycle.ps1:5  Beta  [lsp exact]\ncycle.ps1:11  Delta  [lsp exact]\n" {
		t.Errorf("callers Gamma:\n%s", got)
	}
	if got := mustOK(t, "callers", "Alpha"); got != "cycle.ps1:8  Gamma  [lsp exact]\ncycle.ps1:15  cycle.ps1 (top level)  [lsp exact]\n" {
		t.Errorf("callers Alpha:\n%s", got)
	}
	if got := mustOK(t, "callees", "Beta"); got != "cycle.ps1:5  Gamma  [lsp exact]\n" {
		t.Errorf("callees Beta:\n%s", got)
	}
	// a file names its top-level calls
	if got := mustOK(t, "callees", "cycle.ps1"); got != "cycle.ps1:15  Alpha  [lsp exact]\n" {
		t.Errorf("callees cycle.ps1:\n%s", got)
	}
	// path:line resolves like show
	if got := mustOK(t, "callees", "cycle.ps1:5"); got != "cycle.ps1:5  Gamma  [lsp exact]\n" {
		t.Errorf("callees cycle.ps1:5:\n%s", got)
	}

	// impact is transitive, depth-limited and cycle-safe
	got := mustOK(t, "impact", "Gamma")
	want := "d1  cycle.ps1:5  Beta -> Gamma  [lsp exact]\n" +
		"d1  cycle.ps1:11  Delta -> Gamma  [lsp exact]\n" +
		"d2  cycle.ps1:2  Alpha -> Beta  [lsp exact]\n" +
		"d3  cycle.ps1:8  Gamma -> Alpha  [lsp exact]\n" +
		"d3  cycle.ps1:15  cycle.ps1 (top level) -> Alpha  [lsp exact]\n"
	if got != want {
		t.Errorf("impact Gamma (default depth 3):\n%s\nwant:\n%s", got, want)
	}
	if got := mustOK(t, "impact", "Gamma", "--depth", "1"); strings.Count(got, "\n") != 2 || strings.Contains(got, "d2") {
		t.Errorf("impact --depth 1:\n%s", got)
	}
	if got := mustOK(t, "impact", "Gamma", "--depth", "9"); got != want {
		t.Errorf("impact --depth 9 must stop at the cycle:\n%s", got)
	}

	// shortest path
	if got := mustOK(t, "path", "Alpha", "Gamma"); got != "cycle.ps1:2  Alpha -> Beta  [lsp exact]\ncycle.ps1:5  Beta -> Gamma  [lsp exact]\n" {
		t.Errorf("path Alpha Gamma:\n%s", got)
	}
	if code, _, errw := cli(t, "path", "Lone", "Gamma"); code != 1 || !strings.Contains(errw, "no call path from Lone to Gamma") {
		t.Errorf("path without a route: %d %q", code, errw)
	}

	// no callers: exit 0, nothing on stdout, a note on stderr
	if code, out, errw := cli(t, "callers", "Lone"); code != 0 || out != "" || !strings.Contains(errw, "no callers of Lone found") {
		t.Errorf("callers Lone: %d %q %q", code, out, errw)
	}
	// ambiguity and unknown names
	if code, _, errw := cli(t, "callers", "Dup"); code != 1 || !strings.Contains(errw, "ambiguous") || !strings.Contains(errw, "d1.ps1:1") {
		t.Errorf("callers Dup: %d %q", code, errw)
	}
	if code, _, errw := cli(t, "callees", "Nope"); code != 1 || !strings.Contains(errw, `no symbol matches "Nope"`) {
		t.Errorf("callees Nope: %d %q", code, errw)
	}
	if code, _, _ := cli(t, "callers"); code != 2 {
		t.Errorf("callers without a symbol: %d", code)
	}

	// JSON
	var cj struct {
		Symbol struct {
			Qualified string `json:"qualified_name"`
			Kind      string `json:"kind"`
		} `json:"symbol"`
		Edges []struct {
			Path, From, To, Source, Confidence string
			Line                               int
			Depth                              int
		} `json:"edges"`
	}
	if err := json.Unmarshal([]byte(mustOK(t, "callers", "Gamma", "--json")), &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Symbol.Qualified != "Gamma" || len(cj.Edges) != 2 || cj.Edges[0].From != "Beta" || cj.Edges[0].Line != 5 || cj.Edges[0].Source != "lsp" || cj.Edges[0].Confidence != "exact" {
		t.Errorf("callers json = %+v", cj)
	}
	cj.Edges = nil
	if err := json.Unmarshal([]byte(mustOK(t, "impact", "Gamma", "--json", "--depth", "2")), &cj); err != nil || len(cj.Edges) != 3 || cj.Edges[2].Depth != 2 {
		t.Errorf("impact json: %v %+v", err, cj)
	}
	var empty struct {
		Edges []any `json:"edges"`
	}
	if err := json.Unmarshal([]byte(mustOK(t, "callees", "Lone", "--json")), &empty); err != nil || empty.Edges == nil || len(empty.Edges) != 0 {
		t.Errorf("empty callees json: %v %+v", err, empty)
	}
	var pj struct {
		From struct {
			Qualified string `json:"qualified_name"`
		} `json:"from"`
		Edges []struct{ From, To string } `json:"edges"`
	}
	if err := json.Unmarshal([]byte(mustOK(t, "path", "Alpha", "Gamma", "--json")), &pj); err != nil || pj.From.Qualified != "Alpha" || len(pj.Edges) != 2 || pj.Edges[1].To != "Gamma" {
		t.Errorf("path json: %v %+v", err, pj)
	}

	// stats counts edges by kind, source and confidence
	out = mustOK(t, "stats")
	for _, w := range []string{"edges:       5", "by kind:   calls 5", "by source: lsp 5", "by conf:   exact 5"} {
		if !strings.Contains(out, w) {
			t.Errorf("stats lacks %q:\n%s", w, out)
		}
	}

	// an alias symbol is followed to its target (the AST extractor writes these; here by hand)
	st, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.DB().Exec(`INSERT INTO symbols(id, file_id, name, qualified_name, kind, start_line, start_col, end_line, end_col, source)
		VALUES (900, (SELECT id FROM files WHERE path='cycle.ps1'), 'gam', 'gam', 'alias', 0, 0, 0, 3, 'ast')`)
	st.DB().Exec(`INSERT INTO edges(src_symbol_id, dst_symbol_id, dst_name, kind, file_id, line, col, source, confidence)
		VALUES (900, (SELECT id FROM symbols WHERE name='Gamma'), 'Gamma', 'alias_of', (SELECT id FROM files WHERE path='cycle.ps1'), 0, 0, 'ast', 'exact')`)
	st.Close()
	code, out, errw := cli(t, "callers", "gam")
	if code != 0 || !strings.Contains(errw, "gam is an alias of Gamma") || !strings.HasPrefix(out, "cycle.ps1:5  Beta") {
		t.Errorf("callers gam: %d %q %q", code, out, errw)
	}
}

func TestCallGraphWithoutEdges(t *testing.T) {
	e := setupOpts(t, "", "refs")
	graphTree(t, e)
	mustOK(t, "index", e.root, "--quiet", "--no-edges")
	code, out, errw := cli(t, "callers", "Gamma")
	if code != 0 || out != "" || !strings.Contains(errw, "no call edges") {
		t.Errorf("callers on an index without edges: %d %q %q", code, out, errw)
	}
	if out := mustOK(t, "stats"); !strings.Contains(out, "edges:       0") {
		t.Errorf("stats:\n%s", out)
	}
	// sweep computes them (edges are on by default)
	if out := mustOK(t, "sweep", "--quiet"); !strings.Contains(out, "edges: 5 written") {
		t.Errorf("sweep: %s", out)
	}
	mustOK(t, "callers", "Gamma")
}
