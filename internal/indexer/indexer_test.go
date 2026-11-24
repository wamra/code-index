package indexer_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/filelock"
	"github.com/wamra/code-index/internal/indexer"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/lsptest"
	"github.com/wamra/code-index/internal/store"
)

// The test binary doubles as the fake language server: a language entry runs
// it with CODE_INDEX_FAKE_LSP_SERVE=1.
func TestMain(m *testing.M) {
	if os.Getenv("CODE_INDEX_FAKE_LSP_SERVE") == "1" {
		lsptest.Main()
		return
	}
	os.Exit(m.Run())
}

func fakeCfg(opts string, exts ...string) config.Config {
	return config.Config{
		Languages: map[string]config.Language{
			"fake": {
				Command:    os.Args[0],
				Args:       []string{"-test.run=^$"},
				Env:        map[string]string{"CODE_INDEX_FAKE_LSP_SERVE": "1", "CODE_INDEX_FAKE_LSP": opts},
				Extensions: exts,
				LanguageID: "fake", StartupTimeoutS: 20, RequestTimeoutS: 10,
			},
		},
	}.Resolved()
}

var psExts = []string{".ps1", ".psm1", ".psd1"}

func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := filepath.EvalSymlinks(dst)
	return r
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func run(t *testing.T, st *store.Store, cfg config.Config, root string, force bool) indexer.Result {
	t.Helper()
	res, err := indexer.Run(context.Background(), st, cfg, indexer.Options{Root: root, Force: force, Log: io.Discard, EmptyRetryDelays: fast})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func scalar(t *testing.T, st *store.Store, q string, args ...any) string {
	t.Helper()
	var s *string
	if err := st.DB().QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if s == nil {
		return ""
	}
	return *s
}

var fast = []time.Duration{time.Millisecond}

const fixturePS = "../../testdata/fixture-ps"

func TestIndexFixtureWithFakeServer(t *testing.T) {
	root := copyTree(t, fixturePS)
	st := openStore(t)
	res := run(t, st, fakeCfg("", psExts...), root, false)
	if res.Scanned != 5 || res.Indexed != 5 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if got := scalar(t, st, `SELECT count(*) FROM files WHERE status='ok'`); got != "5" {
		t.Errorf("ok files = %s", got)
	}
	sig := scalar(t, st, `SELECT signature FROM symbols WHERE name='Get-Widget'`)
	doc := scalar(t, st, `SELECT doc FROM symbols WHERE name='Get-Widget'`)
	if sig != "function Get-Widget" {
		t.Errorf("signature = %q (from hover)", sig)
	}
	if !strings.Contains(doc, "Returns a widget by name.") {
		t.Errorf("doc = %q (comment-based help fallback)", doc)
	}
	chunk := scalar(t, st, `SELECT c.text FROM chunks c JOIN symbols s ON c.target_kind='symbol' AND c.target_id=s.id WHERE s.name='Get-Widget'`)
	want := "function Get-Widget\nfile: Widgets/Widgets.psm1\nsignature: function Get-Widget\ndoc: Returns a widget by name."
	if !strings.HasPrefix(chunk, want) {
		t.Errorf("chunk text:\n%s\nwant prefix:\n%s", chunk, want)
	}
	fileChunk := scalar(t, st, `SELECT text FROM chunks WHERE target_kind='file' AND file_id=(SELECT id FROM files WHERE path='Widgets/Widgets.psm1')`)
	if !strings.Contains(fileChunk, "file Widgets/Widgets.psm1") || !strings.Contains(fileChunk, "Get-Widget (function)") || !strings.Contains(fileChunk, "New-Widget (function)") {
		t.Errorf("file chunk = %q", fileChunk)
	}
	// FTS rows track their tables 1:1
	if a, b := scalar(t, st, `SELECT count(*) FROM symbols`), scalar(t, st, `SELECT count(*) FROM name_fts`); a != b {
		t.Errorf("symbols %s vs name_fts %s", a, b)
	}
	if a, b := scalar(t, st, `SELECT count(*) FROM chunks`), scalar(t, st, `SELECT count(*) FROM chunk_fts`); a != b {
		t.Errorf("chunks %s vs chunk_fts %s", a, b)
	}
	if got := scalar(t, st, `SELECT count(*) FROM edges`) + scalar(t, st, `SELECT count(*) FROM summaries`); got != "00" {
		t.Errorf("edges and summaries must stay empty in milestone 1, got %s", got)
	}
	if got := scalar(t, st, `SELECT value FROM meta WHERE key='root'`); got != root {
		t.Errorf("meta root = %q", got)
	}
}

func TestIncrementalSweep(t *testing.T) {
	root := copyTree(t, fixturePS)
	st := openStore(t)
	cfg := fakeCfg("", psExts...)
	run(t, st, cfg, root, false)

	opens := 0
	counting := func(s lsp.Spec) (*lsp.Client, error) { opens++; return lsp.Start(s) }
	do := func(force bool) indexer.Result {
		res, err := indexer.Run(context.Background(), st, cfg, indexer.Options{Root: root, Force: force, Log: io.Discard, Open: counting, EmptyRetryDelays: fast})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := do(false); res.Unchanged != 5 || res.Indexed != 0 || opens != 0 {
		t.Fatalf("nothing changed: %+v opens=%d (no server may start)", res, opens)
	}

	// touching a file (new mtime, same bytes) does not re-index it or start a server
	touch := filepath.Join(root, "lib.ps1")
	future := time.Now().Add(time.Hour)
	os.Chtimes(touch, future, future)
	if res := do(false); res.Unchanged != 5 || opens != 0 {
		t.Fatalf("touch: %+v opens=%d", res, opens)
	}

	// editing one file re-indexes only that file
	os.WriteFile(touch, []byte("function Invoke-Renamed {\n}\n"), 0o644)
	res := do(false)
	if res.Indexed != 1 || res.Unchanged != 4 || opens != 1 {
		t.Fatalf("edit: %+v opens=%d", res, opens)
	}
	if scalar(t, st, `SELECT count(*) FROM symbols WHERE name='Invoke-Helper'`) != "0" || scalar(t, st, `SELECT count(*) FROM symbols WHERE name='Invoke-Renamed'`) != "1" {
		t.Error("old symbols of the edited file must be replaced")
	}

	// deleting a file drops its rows everywhere
	os.Remove(filepath.Join(root, "main.ps1"))
	res = do(false)
	if res.Deleted != 1 {
		t.Fatalf("delete: %+v", res)
	}
	for _, q := range []string{
		`SELECT count(*) FROM files WHERE path='main.ps1'`,
		`SELECT count(*) FROM symbols WHERE file_id NOT IN (SELECT id FROM files)`,
		`SELECT count(*) FROM chunks WHERE file_id NOT IN (SELECT id FROM files)`,
	} {
		if scalar(t, st, q) != "0" {
			t.Errorf("%s != 0", q)
		}
	}
	if a, b := scalar(t, st, `SELECT count(*) FROM chunks`), scalar(t, st, `SELECT count(*) FROM chunk_fts`); a != b {
		t.Errorf("chunk_fts out of sync after delete: %s vs %s", a, b)
	}

	// a file that becomes ignored is dropped too
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("bad.ps1\n"), 0o644)
	if res := do(false); res.Deleted != 1 || scalar(t, st, `SELECT count(*) FROM files WHERE path='bad.ps1'`) != "0" {
		t.Fatalf("ignored: %+v", res)
	}

	// --force re-indexes everything
	opens = 0
	if res := do(true); res.Indexed != 3 || opens != 1 {
		t.Fatalf("force: %+v opens=%d", res, opens)
	}
}

func TestFlatSymbolInformationAndHierarchy(t *testing.T) {
	src := "# Opens things.\nclass Box {\n  function Open {\n  }\n}\n"
	for _, flat := range []bool{false, true} {
		root := tree(t, map[string]string{"a.ps1": src})
		st := openStore(t)
		opts := ""
		if flat {
			opts = "flat"
		}
		res := run(t, st, fakeCfg(opts, ".ps1"), root, false)
		if res.Indexed != 1 || res.Symbols != 2 {
			t.Fatalf("flat=%v: %+v", flat, res)
		}
		if got := scalar(t, st, `SELECT qualified_name FROM symbols WHERE name='Open'`); got != "Box.Open" {
			t.Errorf("flat=%v: qualified name = %q", flat, got)
		}
		if got := scalar(t, st, `SELECT p.name FROM symbols s JOIN symbols p ON p.id=s.parent_id WHERE s.name='Open'`); got != "Box" {
			t.Errorf("flat=%v: parent = %q", flat, got)
		}
		if got := scalar(t, st, `SELECT doc FROM symbols WHERE name='Box'`); got != "Opens things." {
			t.Errorf("flat=%v: doc = %q", flat, got)
		}
	}
}

func tree(t *testing.T, files map[string]string) string {
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
	r, _ := filepath.EvalSymlinks(root)
	return r
}

func TestFileFailureIsRecordedAndRetried(t *testing.T) {
	root := tree(t, map[string]string{"good.ps1": "function Good {\n}\n", "broken.ps1": "function Broken {\n}\n"})
	st := openStore(t)
	res := run(t, st, fakeCfg("failpath=broken", ".ps1"), root, false)
	if res.Indexed != 1 || res.Failed != 1 || len(res.FileErrors) != 1 || !strings.HasPrefix(res.FileErrors[0], "broken.ps1: ") {
		t.Fatalf("result = %+v", res)
	}
	if got := scalar(t, st, `SELECT status FROM files WHERE path='broken.ps1'`); got != "failed" {
		t.Errorf("status = %q", got)
	}
	if got := scalar(t, st, `SELECT fail_reason FROM files WHERE path='broken.ps1'`); !strings.Contains(got, "internal error in fake server") {
		t.Errorf("fail_reason = %q", got)
	}
	// the server is fine now: the failed file is retried although its bytes did not change
	res = run(t, st, fakeCfg("", ".ps1"), root, false)
	if res.Indexed != 1 || res.Unchanged != 1 || res.Failed != 0 {
		t.Fatalf("retry: %+v", res)
	}
	if got := scalar(t, st, `SELECT status FROM files WHERE path='broken.ps1'`); got != "ok" {
		t.Errorf("status after retry = %q", got)
	}
}

func TestServerQuirks(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": "function Alpha {\n}\n", "b.ps1": "function Beta {\n}\n"})
	st := openStore(t)
	// junk on stdout, "server is starting" answers, an empty first answer and
	// a workspace/configuration request, all at once
	res := run(t, st, fakeCfg("junk,starting=2,emptyfirst=1,config=2", ".ps1"), root, false)
	if res.Indexed != 2 || res.Symbols != 2 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestEmptyAnswerRetryStopsWhenItNeverHelps(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		files[n+".ps1"] = "# nothing declared here\n"
	}
	root := tree(t, files)
	st := openStore(t)
	res := run(t, st, fakeCfg("", ".ps1"), root, false)
	if res.Indexed != 5 || res.Symbols != 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestMissingServerAndNoDocumentSymbol(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": "function A {\n}\n"})
	st := openStore(t)
	cfg := config.Config{Languages: map[string]config.Language{
		"fake": {Command: "/nonexistent/language-server", Extensions: []string{".ps1"}},
	}}.Resolved()
	res := run(t, st, cfg, root, false)
	if res.Failed != 1 || res.LangErrors["fake"] == "" || res.Indexed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if scalar(t, st, `SELECT count(*) FROM files`) != "0" {
		t.Error("a language that cannot start must leave the index untouched")
	}
	res = run(t, st, fakeCfg("nodocsymbol", ".ps1"), root, false)
	if !strings.Contains(res.LangErrors["fake"], "does not advertise documentSymbol") {
		t.Fatalf("LangErrors = %v", res.LangErrors)
	}
}

func TestNoHoverStillIndexesWithFallbacks(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": "# Adds numbers.\nfunction Add-Numbers {\n}\n"})
	st := openStore(t)
	res := run(t, st, fakeCfg("nohover", ".ps1"), root, false)
	if res.Indexed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := scalar(t, st, `SELECT signature FROM symbols WHERE name='Add-Numbers'`); got != "function Add-Numbers" {
		t.Errorf("signature fallback = %q (declaration line)", got)
	}
	if got := scalar(t, st, `SELECT doc FROM symbols WHERE name='Add-Numbers'`); got != "Adds numbers." {
		t.Errorf("doc fallback = %q (comment above)", got)
	}
}

func TestOtherWorkspaceAndLock(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": "function A {\n}\n"})
	st := openStore(t)
	cfg := fakeCfg("", ".ps1")
	run(t, st, cfg, root, false)
	_, err := indexer.Run(context.Background(), st, cfg, indexer.Options{Root: t.TempDir(), Log: io.Discard, EmptyRetryDelays: fast})
	if err == nil || !strings.Contains(err.Error(), "indexes "+root) {
		t.Errorf("indexing another root into the same database: %v", err)
	}
	l, err := filelock.Try(filepath.Join(filepath.Dir(st.Path()), "index.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	_, err = indexer.Run(context.Background(), st, cfg, indexer.Options{Root: root, Log: io.Discard, EmptyRetryDelays: fast})
	if !errors.Is(err, indexer.ErrLocked) {
		t.Errorf("concurrent run: %v, want ErrLocked", err)
	}
}

func TestCRLFAndBOMSourcesIndexLikeLF(t *testing.T) {
	lf := "# Adds numbers.\nfunction Add-Numbers {\n  <#\n  .SYNOPSIS\n  Sums two values.\n  #>\n}\n"
	crlf := "\ufeff" + strings.ReplaceAll(lf, "\n", "\r\n")
	for name, src := range map[string]string{"lf": lf, "crlf+bom": crlf} {
		root := tree(t, map[string]string{"a.ps1": src})
		st := openStore(t)
		res := run(t, st, fakeCfg("nohover", ".ps1"), root, false)
		if res.Indexed != 1 || res.Failed != 0 {
			t.Fatalf("%s: %+v", name, res)
		}
		if got := scalar(t, st, `SELECT signature FROM symbols WHERE name='Add-Numbers'`); got != "function Add-Numbers" {
			t.Errorf("%s: signature = %q", name, got)
		}
		if got := scalar(t, st, `SELECT doc FROM symbols WHERE name='Add-Numbers'`); got != "Adds numbers." {
			t.Errorf("%s: doc = %q", name, got)
		}
		if got := scalar(t, st, `SELECT start_line FROM symbols WHERE name='Add-Numbers'`); got != "1" {
			t.Errorf("%s: start_line = %s", name, got)
		}
	}
}

// A language whose server cannot start reports the language error only: no
// extra note about the server.
func TestMissingServerGivesNoExtraNote(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": "function A {\n}\n"})
	cfg := config.Config{Languages: map[string]config.Language{
		"fake": {Command: "/nonexistent/language-server", Extensions: []string{".ps1"}},
	}}.Resolved()
	res := run(t, openStore(t), cfg, root, false)
	if len(res.Notes) != 0 || res.LangErrors["fake"] == "" {
		t.Errorf("notes = %v, lang errors = %v", res.Notes, res.LangErrors)
	}
}
