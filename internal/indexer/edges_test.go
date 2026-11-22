package indexer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/indexer"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/store"
)

func runOpts(t *testing.T, st *store.Store, cfg config.Config, o indexer.Options) indexer.Result {
	t.Helper()
	o.Log, o.EmptyRetryDelays = io.Discard, fast
	res, err := indexer.Run(context.Background(), st, cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// edgeLines lists the calls edges as "file:line caller -> callee [source confidence]" (1-based lines).
func edgeLines(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT f.path, e.line+1, COALESCE(ss.qualified_name, '(file)'), COALESCE(ds.qualified_name, e.dst_name), e.source, e.confidence
		FROM edges e JOIN files f ON f.id=e.file_id LEFT JOIN symbols ss ON ss.id=e.src_symbol_id LEFT JOIN symbols ds ON ds.id=e.dst_symbol_id
		WHERE e.kind='calls' ORDER BY f.path, e.line, e.col`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p, src, dst, source, conf string
		var line int
		if err := rows.Scan(&p, &line, &src, &dst, &source, &conf); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s:%d %s -> %s [%s %s]", p, line, src, dst, source, conf))
	}
	return out
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("edges:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const (
	srcA = "function Alpha {\n  Beta\n}\n"
	srcB = "function Beta {\n}\nfunction Gamma {\n  Beta\n  Beta\n}\nBeta\n"
)

func TestLSPReferencesBecomeCallsEdges(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	cfg := fakeCfg("refs", ".ps1")
	res := runOpts(t, st, cfg, indexer.Options{Root: root})
	if res.Edges != 4 || res.EdgeLangs != 1 {
		t.Fatalf("result = %+v", res)
	}
	// the declaration is skipped; a reference at the top level has the file as caller
	wantLines(t, edgeLines(t, st),
		"a.ps1:2 Alpha -> Beta [lsp exact]",
		"b.ps1:4 Gamma -> Beta [lsp exact]",
		"b.ps1:5 Gamma -> Beta [lsp exact]",
		"b.ps1:7 (file) -> Beta [lsp exact]")
	if got := scalar(t, st, `SELECT count(*) FROM edges WHERE src_symbol_id IS NULL AND file_id=(SELECT id FROM files WHERE path='b.ps1')`); got != "1" {
		t.Errorf("top-level edges in b.ps1 = %s", got)
	}

	// chunk text gains the calls / called by lines
	chunk := func(name string) string {
		return scalar(t, st, `SELECT c.text FROM chunks c JOIN symbols s ON c.target_kind='symbol' AND c.target_id=s.id WHERE s.name=?`, name)
	}
	if got := chunk("Beta"); !strings.HasSuffix(got, "\ncalled by: Alpha, Gamma, b.ps1") {
		t.Errorf("Beta chunk:\n%s", got)
	}
	if got := chunk("Gamma"); !strings.HasSuffix(got, "\ncalls: Beta") {
		t.Errorf("Gamma chunk:\n%s", got)
	}
	if got := chunk("Alpha"); !strings.Contains(got, "calls: Beta") {
		t.Errorf("Alpha chunk:\n%s", got)
	}
	if a, b := scalar(t, st, `SELECT count(*) FROM chunks`), scalar(t, st, `SELECT count(*) FROM chunk_fts`); a != b {
		t.Errorf("chunk_fts out of sync: chunks %s fts %s", a, b)
	}
	if got := scalar(t, st, `SELECT count(*) FROM chunk_fts WHERE chunk_fts MATCH '"called by"'`); got == "0" {
		t.Error("the called-by line must be full-text searchable")
	}

	// nothing changed: no server starts and nothing is recomputed
	opens := 0
	counting := func(s lsp.Spec) (*lsp.Client, error) { opens++; return lsp.Start(s) }
	if res := runOpts(t, st, cfg, indexer.Options{Root: root, Open: counting}); res.EdgeLangs != 0 || opens != 0 {
		t.Fatalf("unchanged run: %+v opens=%d", res, opens)
	}

	// pretend everything is embedded, then drop Alpha's call: only chunks whose text changed go back to pending
	if _, err := st.DB().Exec(`UPDATE chunks SET embedded_at='2026-01-01T00:00:00Z', embed_fingerprint='x'`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.ps1"), []byte("function Alpha {\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = runOpts(t, st, cfg, indexer.Options{Root: root, Open: counting})
	if res.Indexed != 1 || opens != 1 || res.Edges != 3 {
		t.Fatalf("edit: %+v opens=%d", res, opens)
	}
	wantLines(t, edgeLines(t, st),
		"b.ps1:4 Gamma -> Beta [lsp exact]",
		"b.ps1:5 Gamma -> Beta [lsp exact]",
		"b.ps1:7 (file) -> Beta [lsp exact]")
	pending := func(name string) string {
		return scalar(t, st, `SELECT c.embedded_at IS NULL FROM chunks c JOIN symbols s ON c.target_kind='symbol' AND c.target_id=s.id WHERE s.name=?`, name)
	}
	if pending("Beta") != "1" || pending("Gamma") != "0" {
		t.Errorf("Beta pending=%s (its called-by changed), Gamma pending=%s (text unchanged)", pending("Beta"), pending("Gamma"))
	}
	if got := chunk("Beta"); !strings.HasSuffix(got, "\ncalled by: Gamma, b.ps1") {
		t.Errorf("Beta chunk after edit:\n%s", got)
	}

	// deleting a file drops its edges and leaves no dangling callee
	os.Remove(filepath.Join(root, "b.ps1"))
	runOpts(t, st, cfg, indexer.Options{Root: root})
	if got := edgeLines(t, st); len(got) != 0 {
		t.Errorf("edges after deleting b.ps1: %v", got)
	}
}

func TestNoEdgesFlagAndStaleRecovery(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	cfg := fakeCfg("refs", ".ps1")
	if res := runOpts(t, st, cfg, indexer.Options{Root: root, NoEdges: true}); res.EdgeLangs != 0 || len(edgeLines(t, st)) != 0 {
		t.Fatalf("--no-edges: %+v", res)
	}
	// a later run without the flag computes them although no file changed
	if res := runOpts(t, st, cfg, indexer.Options{Root: root}); res.Edges != 4 || res.Indexed != 0 {
		t.Fatalf("catch-up: %+v", res)
	}
	// an edit under --no-edges leaves the old edges stale; the next run rebuilds them
	os.WriteFile(filepath.Join(root, "a.ps1"), []byte("function Alpha {\n}\n"), 0o644)
	runOpts(t, st, cfg, indexer.Options{Root: root, NoEdges: true})
	if res := runOpts(t, st, cfg, indexer.Options{Root: root}); res.Edges != 3 {
		t.Fatalf("recovery: %+v", res)
	}
}

func TestReferencesNotAdvertisedOrFailing(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	res := runOpts(t, st, fakeCfg("", ".ps1"), indexer.Options{Root: root})
	if res.Edges != 0 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "does not advertise references") {
		t.Fatalf("no capability: %+v", res)
	}
	st2 := openStore(t)
	res = runOpts(t, st2, fakeCfg("refsfail", ".ps1"), indexer.Options{Root: root})
	if res.Edges != 0 || res.Indexed != 2 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "references request(s) failed") {
		t.Fatalf("failing references: %+v", res)
	}
}

func TestMutualRecursionEdges(t *testing.T) {
	root := tree(t, map[string]string{"c.ps1": "function A {\n  B\n}\nfunction B {\n  A\n}\n"})
	st := openStore(t)
	runOpts(t, st, fakeCfg("refs", ".ps1"), indexer.Options{Root: root})
	wantLines(t, edgeLines(t, st), "c.ps1:2 A -> B [lsp exact]", "c.ps1:5 B -> A [lsp exact]")
}

// astSample serves the recorded extractor output for the fixture, with the
// file names made absolute under the root of the workspace being indexed.
func astSample(t *testing.T, root string) indexer.ExtractFunc {
	return func(_ context.Context, name string, files []string) ([]byte, error) {
		if name != indexer.ExtractorPowerShellAST {
			t.Errorf("extractor name = %q", name)
		}
		for _, f := range files {
			if ext := filepath.Ext(f); ext != ".ps1" && ext != ".psm1" {
				t.Errorf("extractor got %s (only .ps1/.psm1 go to the AST extractor)", f)
			}
		}
		b, err := os.ReadFile("testdata/powershell-ast.sample.jsonl")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				t.Fatal(err)
			}
			m["file"] = filepath.Join(root, filepath.FromSlash(m["file"].(string)))
			enc, _ := json.Marshal(m)
			out.Write(enc)
			out.WriteByte('\n')
		}
		return out.Bytes(), nil
	}
}

func astCfg(opts string) config.Config {
	cfg := fakeCfg(opts, psExts...)
	l := cfg.Languages["fake"]
	l.Extractor = indexer.ExtractorPowerShellAST
	cfg.Languages["fake"] = l
	return cfg
}

func TestASTEdgesAliasesDynamicAndMerge(t *testing.T) {
	root := copyTree(t, fixturePS)
	st := openStore(t)
	cfg := astCfg("refs")
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: astSample(t, root)})
	if res.Aliases != 1 || res.Failed != 0 || len(res.Notes) != 0 {
		t.Fatalf("result = %+v", res)
	}
	all := strings.Join(edgeLines(t, st), "\n")
	for _, want := range []string{
		"main.ps1:3 (file) -> Get-Widget [lsp+ast exact]", // found by both: one row
		"lib.ps1:10 Invoke-Helper -> Get-Widget [lsp+ast exact]",
		"main.ps1:4 (file) -> Get-Widget [ast alias]", // gw resolved to its target
		"main.ps1:5 (file) -> Invoke-Helper [lsp+ast exact]",
		"main.ps1:9 (file) -> & $cmd [ast dynamic]", // dynamic call with its expression
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	for _, bad := range []string{"Join-Path", "Write-Output", "Export-ModuleMember"} {
		if strings.Contains(all, bad) {
			t.Errorf("built-in command %s must not become an edge:\n%s", bad, all)
		}
	}
	if got := scalar(t, st, `SELECT count(*) FROM edges WHERE kind='calls' AND source='lsp+ast' AND file_id=(SELECT id FROM files WHERE path='main.ps1') AND line=2`); got != "1" {
		t.Errorf("merged rows for main.ps1:3 = %s, want 1", got)
	}

	// alias symbol and alias_of edge
	if got := scalar(t, st, `SELECT s.kind || ' ' || s.source || ' ' || s.signature FROM symbols s WHERE s.name='gw'`); got != "alias ast gw -> Get-Widget" {
		t.Errorf("alias symbol = %q", got)
	}
	if got := scalar(t, st, `SELECT ds.name || ' ' || e.confidence || ' ' || e.source FROM edges e JOIN symbols ds ON ds.id=e.dst_symbol_id WHERE e.kind='alias_of'`); got != "Get-Widget exact ast" {
		t.Errorf("alias_of = %q", got)
	}
	if got := scalar(t, st, `SELECT text FROM chunks WHERE target_kind='symbol' AND target_id=(SELECT id FROM symbols WHERE name='gw')`); !strings.HasPrefix(got, "alias gw\nfile: Widgets/Widgets.psm1\nsignature: gw -> Get-Widget") {
		t.Errorf("alias chunk = %q", got)
	}
	if got := scalar(t, st, `SELECT count(*) FROM name_fts WHERE rowid=(SELECT id FROM symbols WHERE name='gw')`); got != "1" {
		t.Errorf("alias in name_fts: %s", got)
	}

	// dot-sourcing and Import-Module resolve to files
	if got := scalar(t, st, `SELECT f.path || ' ' || e.kind || ' ' || e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.kind='dot_sources'`); got != "main.ps1 dot_sources lib.ps1" {
		t.Errorf("dot_sources = %q", got)
	}
	if got := scalar(t, st, `SELECT f.path || ' ' || e.kind || ' ' || e.dst_name FROM edges e JOIN files f ON f.id=e.file_id WHERE e.kind='imports'`); got != "lib.ps1 imports Widgets/Widgets.psd1" {
		t.Errorf("imports = %q", got)
	}

	// the alias call shows up in Get-Widget's chunk as a caller; a forced re-run neither duplicates aliases nor edges
	n := scalar(t, st, `SELECT count(*) FROM edges`)
	res = runOpts(t, st, cfg, indexer.Options{Root: root, Force: true, Extract: astSample(t, root)})
	if got := scalar(t, st, `SELECT count(*) FROM symbols WHERE kind='alias'`); got != "1" || res.Aliases != 1 {
		t.Errorf("aliases after --force = %s (%+v)", got, res)
	}
	if got := scalar(t, st, `SELECT count(*) FROM edges`); got != n {
		t.Errorf("edges after --force = %s, before %s", got, n)
	}
	// a touched file's edges are rebuilt although the others did not change
	os.WriteFile(filepath.Join(root, "bad.ps1"), []byte("function Broken-Thing {\n}\n"), 0o644)
	res = runOpts(t, st, cfg, indexer.Options{Root: root, Extract: astSample(t, root)})
	if res.Indexed != 1 || res.Aliases != 1 || scalar(t, st, `SELECT count(*) FROM edges`) != n {
		t.Errorf("incremental: %+v edges=%s want %s", res, scalar(t, st, `SELECT count(*) FROM edges`), n)
	}
}

func TestASTExtractorMissingIsSkippedWithOneNote(t *testing.T) {
	root := copyTree(t, fixturePS)
	st := openStore(t)
	missing := func(context.Context, string, []string) ([]byte, error) {
		return nil, fmt.Errorf("%w: neither pwsh nor powershell is on PATH", indexer.ErrExtractorUnavailable)
	}
	res := runOpts(t, st, astCfg("refs"), indexer.Options{Root: root, Extract: missing})
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "neither pwsh nor powershell is on PATH") || !strings.Contains(res.Notes[0], "skipping the AST extractor") {
		t.Fatalf("notes = %v", res.Notes)
	}
	if res.Aliases != 0 || res.Edges == 0 || res.Failed != 0 {
		t.Errorf("LSP edges must still be written: %+v", res)
	}
	if got := scalar(t, st, `SELECT count(*) FROM edges WHERE source <> 'lsp'`); got != "0" {
		t.Errorf("non-lsp edges = %s", got)
	}
}

func TestASTExtractorFailureKeepsIndexAndRetries(t *testing.T) {
	root := copyTree(t, fixturePS)
	st := openStore(t)
	boom := func(context.Context, string, []string) ([]byte, error) { return nil, errors.New("boom") }
	res := runOpts(t, st, astCfg("refs"), indexer.Options{Root: root, Extract: boom})
	if res.Indexed != 5 || res.EdgeLangs != 0 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "edges not computed") {
		t.Fatalf("result = %+v", res)
	}
	// not marked done: the next run computes them without re-indexing
	res = runOpts(t, st, astCfg("refs"), indexer.Options{Root: root, Extract: astSample(t, root)})
	if res.Indexed != 0 || res.Aliases != 1 || res.Edges == 0 {
		t.Fatalf("retry: %+v", res)
	}
}
