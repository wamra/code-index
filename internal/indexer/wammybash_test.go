package indexer_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/extractest"
	"github.com/wamra/code-index/internal/indexer"
	"github.com/wamra/code-index/internal/store"
)

const (
	fixtureBash  = "../../testdata/fixture-bash-wammybash"
	wammybashOut = "../extract/testdata/wammybash-extract.golden.jsonl"
)

// bashExecCfg is a language served only by an exec extractor entry for
// wammybash: no language server, no command resolved (the test replaces the
// subprocess).
func bashExecCfg() config.Config {
	return config.Config{
		Languages: map[string]config.Language{
			"bash": {Extensions: []string{".sh", ".bash"}, Extractor: "wammybash"},
		},
		Extractors: map[string]config.Extractor{
			"wammybash": {
				Type:        config.ExtractorExec,
				Argv:        []string{"wammybash", "extract", "--root", "{root}", "--list-file", "{listFile}"},
				VersionArgv: []string{"wammybash", "version"},
			},
		},
	}.Resolved()
}

// goldenExtract serves the recorded `wammybash extract` output of the corpus
// for the requested files, with <root>/ replaced by the workspace root.
func goldenExtract(t *testing.T, root string) indexer.ExtractFunc {
	return func(_ context.Context, name string, files []string) ([]byte, error) {
		if name != "wammybash" {
			t.Errorf("extractor name = %q", name)
		}
		golden, err := os.ReadFile(wammybashOut)
		if err != nil {
			t.Fatal(err)
		}
		return extractest.Rewrite(golden, root, files)
	}
}

// callRows lists every calls edge as "file:line caller -> target [source confidence reason]"
// where target is "path:name" of the destination symbol (the file tells two
// same-named functions apart).
func callRows(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT f.path, e.line+1, COALESCE(ss.qualified_name, '(file)'),
		COALESCE(df.path || ':' || ds.name, e.dst_name), e.source, e.confidence, COALESCE(e.reason, '')
		FROM edges e JOIN files f ON f.id=e.file_id
		LEFT JOIN symbols ss ON ss.id=e.src_symbol_id
		LEFT JOIN symbols ds ON ds.id=e.dst_symbol_id LEFT JOIN files df ON df.id=ds.file_id
		WHERE e.kind='calls' ORDER BY f.path, e.line, e.col`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p, src, dst, source, conf, reason string
		var line int
		if err := rows.Scan(&p, &line, &src, &dst, &source, &conf, &reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s:%d %s -> %s [%s %s %s]", p, line, src, dst, source, conf, reason))
	}
	return out
}

// A language with only an exec extractor is indexed end to end through the
// Options.Extract seam, from the recorded output of wammybash: symbols from
// its func records, edges with the file each call goes to.
func TestExecOnlyBashLanguageFromWammybashGolden(t *testing.T) {
	root := copyTree(t, fixtureBash)
	st := openStore(t)
	cfg := bashExecCfg()
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: goldenExtract(t, root)})
	if res.Scanned != 9 || res.Indexed != 9 || res.Failed != 0 || len(res.LangErrors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.EdgeLangs != 1 || res.Aliases != 1 {
		t.Fatalf("edges: %+v", res)
	}

	// functions are symbols (source extract) with signature and doc taken from the source text
	if got := scalar(t, st, `SELECT kind || '|' || source || '|' || signature || '|' || COALESCE(doc,'') FROM symbols WHERE name='log'`); got != "function|extract|log() { printf '%s\\n' \"$*\" >&2; }|Log a message to stderr." {
		t.Errorf("log symbol = %q", got)
	}
	if got := scalar(t, st, `SELECT count(*) FROM symbols WHERE name='build_release' AND kind='function'`); got != "2" {
		t.Errorf("build_release symbols = %s, want 2 (lib/util.sh and orphan.sh)", got)
	}
	if got := scalar(t, st, `SELECT start_line || '-' || end_line FROM symbols WHERE name='deploy'`); got != "3-11" {
		t.Errorf("deploy range = %s (zero-based lines)", got)
	}

	rows := callRows(t, st)
	all := strings.Join(rows, "\n")
	for _, want := range []string{
		// resolved through "to": lib/util.sh, never orphan.sh
		"main.sh:6 deploy -> lib/util.sh:build_release [ast exact direct]",
		"main.sh:7 deploy -> lib/util.sh:build_release [ast alias alias]",
		"main.sh:9 deploy -> lib/util.sh:build_release [ast candidate variable-constant]",
		"main.sh:10 deploy -> lib/util.sh:build_release [ast candidate string-eval]",
		"main.sh:11 deploy -> lib/util.sh:log [ast candidate string-trap]",
		"main.sh:21 (file) -> main.sh:deploy [ast exact direct]",
		"dyn.sh:4 (file) -> lib/util.sh:build_release [ast exact direct]",
		"orphan.sh:3 (file) -> orphan.sh:build_release [ast exact direct]",
		"lib/util.sh:6 build_release -> lib/util.sh:log [ast exact direct]",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if got := scalar(t, st, `SELECT count(*) FROM edges e JOIN files f ON f.id=e.file_id JOIN symbols ds ON ds.id=e.dst_symbol_id JOIN files df ON df.id=ds.file_id
		WHERE e.kind='calls' AND df.path='orphan.sh' AND f.path <> 'orphan.sh'`); got != "0" {
		t.Errorf("%s call(s) from another file attributed to orphan.sh's build_release", got)
	}

	// the alias record and the sources
	if got := scalar(t, st, `SELECT kind || ' ' || source || ' ' || signature FROM symbols WHERE name='br'`); got != "alias ast br -> build_release" {
		t.Errorf("alias symbol = %q", got)
	}
	if got := scalar(t, st, `SELECT count(*) FROM edges WHERE kind='dot_sources'`); got == "0" {
		t.Error("source commands must give dot_sources edges")
	}

	// a second run is a no-op (the fingerprint is stored) and rebuilds nothing
	extractRuns := 0
	counting := goldenExtract(t, root)
	again := indexer.Options{Root: root, Extract: func(ctx context.Context, name string, files []string) ([]byte, error) {
		extractRuns++
		return counting(ctx, name, files)
	}}
	if res := runOpts(t, st, cfg, again); res.Indexed != 0 || res.Unchanged != 9 || res.EdgeLangs != 0 || extractRuns != 0 {
		t.Errorf("second run: %+v, extractor runs %d", res, extractRuns)
	}
}

// One extractor run serves both phases of the first index (same files).
func TestExecOnlyLanguageRunsTheExtractorOncePerFileSet(t *testing.T) {
	root := copyTree(t, fixtureBash)
	st := openStore(t)
	runs := 0
	seam := goldenExtract(t, root)
	runOpts(t, st, bashExecCfg(), indexer.Options{Root: root, Extract: func(ctx context.Context, name string, files []string) ([]byte, error) {
		runs++
		return seam(ctx, name, files)
	}})
	if runs != 1 {
		t.Errorf("extractor ran %d times for the first index, want 1", runs)
	}
}

// An error record fails the file; the others are still indexed, and the failed
// one is retried on the next run.
func TestExecSymbolsErrorRecordFailsOnlyThatFile(t *testing.T) {
	root := copyTree(t, fixtureBash)
	st := openStore(t)
	seam := goldenExtract(t, root)
	bad := func(ctx context.Context, name string, files []string) ([]byte, error) {
		out, err := seam(ctx, name, files)
		if err != nil {
			return nil, err
		}
		line := fmt.Sprintf(`{"t":"error","file":%q,"message":"zsh is not supported yet"}`+"\n", root+"/orphan.sh")
		return append(out, line...), nil
	}
	res := runOpts(t, st, bashExecCfg(), indexer.Options{Root: root, Extract: bad})
	if res.Indexed != 8 || res.Failed != 1 || len(res.FileErrors) != 1 || !strings.HasPrefix(res.FileErrors[0], "orphan.sh: zsh is not supported yet") {
		t.Fatalf("result = %+v", res)
	}
	if got := scalar(t, st, `SELECT status FROM files WHERE path='orphan.sh'`); got != "failed" {
		t.Errorf("status = %q", got)
	}
	res = runOpts(t, st, bashExecCfg(), indexer.Options{Root: root, Extract: goldenExtract(t, root)})
	if res.Indexed != 1 || res.Failed != 0 {
		t.Errorf("retry: %+v", res)
	}
}

// A file the extractor never emits fails with a reason; a returned error fails
// the files not yet emitted, marks the language unusable and computes no edges.
func TestExecSymbolsMissingFileAndReturnedError(t *testing.T) {
	root := copyTree(t, fixtureBash)
	st := openStore(t)
	seam := goldenExtract(t, root)
	// the subprocess runs once and its output is reused: drop one file's records
	// and the "emitted for every requested file" rule still gives it an empty list,
	// so it is indexed with no symbols rather than failed
	quiet := func(ctx context.Context, name string, files []string) ([]byte, error) {
		return seam(ctx, name, files[:len(files)-1])
	}
	res := runOpts(t, st, bashExecCfg(), indexer.Options{Root: root, Extract: quiet})
	if res.Indexed != 9 || res.Failed != 0 {
		t.Fatalf("a file without records is an empty file: %+v", res)
	}

	root2 := copyTree(t, fixtureBash)
	st2 := openStore(t)
	boom := func(context.Context, string, []string) ([]byte, error) { return nil, fmt.Errorf("extractor exploded") }
	res = runOpts(t, st2, bashExecCfg(), indexer.Options{Root: root2, Extract: boom})
	if res.Indexed != 0 || res.Failed != 9 || res.EdgeLangs != 0 || !strings.Contains(res.LangErrors["bash"], "extractor exploded") {
		t.Fatalf("returned error: %+v", res)
	}
	if got := scalar(t, st2, `SELECT count(*) FROM files WHERE status='failed'`); got != "9" {
		t.Errorf("failed files = %s", got)
	}
	if got := scalar(t, st2, `SELECT count(*) FROM edges`); got != "0" {
		t.Errorf("edges after a failed language = %s", got)
	}
}

// With an exec-only language and no symbols provider the language is unusable with a clear reason.
func TestExecWithoutSymbolsProvider(t *testing.T) {
	root := copyTree(t, fixtureBash)
	st := openStore(t)
	cfg := bashExecCfg()
	no := false
	ex := cfg.Extractors["wammybash"]
	ex.Symbols = &no
	cfg.Extractors["wammybash"] = ex
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: goldenExtract(t, root)})
	if res.Indexed != 0 || res.Failed != 9 || !strings.Contains(res.LangErrors["bash"], "does not provide symbols") {
		t.Fatalf("result = %+v", res)
	}
}

// A changed file outside the symbols extractor's extensions is stored without
// symbols, and the extractor is not run for an empty list.
func TestFilesOutsideSymbolExtensionsAreStoredEmpty(t *testing.T) {
	root := tree(t, map[string]string{"a.sh": "f() { :; }\n", "b.bash": "g() { :; }\n"})
	cfg := bashExecCfg()
	ex := cfg.Extractors["wammybash"]
	ex.Extensions = []string{".sh"}
	cfg.Extractors["wammybash"] = ex
	st := openStore(t)
	runs := 0
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: func(_ context.Context, _ string, files []string) ([]byte, error) {
		runs++
		var out []string
		for _, f := range files {
			if strings.HasSuffix(f, "b.bash") {
				t.Errorf("b.bash must not reach the extractor: %v", files)
			}
			out = append(out, fmt.Sprintf(`{"t":"func","file":%q,"name":"f","line":1,"col":1,"endLine":1,"endCol":11}`, f))
		}
		return []byte(strings.Join(out, "\n") + "\n"), nil
	}})
	if res.Indexed != 2 || res.Failed != 0 || res.Symbols != 1 || runs != 1 {
		t.Fatalf("result = %+v, runs %d", res, runs)
	}
	if got := scalar(t, st, `SELECT count(*) FROM files WHERE path='b.bash' AND status='ok'`); got != "1" {
		t.Errorf("b.bash status ok rows = %s", got)
	}
}
