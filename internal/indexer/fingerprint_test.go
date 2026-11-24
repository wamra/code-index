package indexer_test

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/indexer"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/store"
)

const edgesKey = "edges_lang:fake"

// noExec is an Extract seam that answers with no records, so no subprocess runs.
func noExec(context.Context, string, []string) ([]byte, error) { return nil, nil }

// withExtractor adds an exec extractor "ext" with a static version to the
// fake language, after the language server (the legacy Command).
func withExtractor(cfg config.Config, version string) config.Config {
	cfg.Extractors = map[string]config.Extractor{
		"ext": {Type: config.ExtractorExec, Argv: []string{"unused-extractor"}, Version: version},
	}
	l := cfg.Languages["fake"]
	l.Extractor = config.NewExtractorNames("ext")
	cfg.Languages["fake"] = l
	return cfg
}

func metaOf(t *testing.T, st *store.Store) string {
	t.Helper()
	v, err := st.Meta(context.Background(), edgesKey)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func isFingerprint(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

func TestEdgeMetaIsFingerprintAndStable(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	cfg := fakeCfg("refs", ".ps1")
	runOpts(t, st, cfg, indexer.Options{Root: root})
	fp := metaOf(t, st)
	if !isFingerprint(fp) {
		t.Fatalf("edges meta = %q, want a 64-character hex fingerprint", fp)
	}

	opens := 0
	counting := func(s lsp.Spec) (*lsp.Client, error) { opens++; return lsp.Start(s) }
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Open: counting})
	if res.Indexed != 0 || res.EdgeLangs != 0 || opens != 0 {
		t.Fatalf("unchanged run: %+v opens=%d", res, opens)
	}
	if again := metaOf(t, st); again != fp {
		t.Errorf("fingerprint changed on an unchanged run: %q then %q", fp, again)
	}
}

func TestExtractorVersionChangeReindexesLanguage(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	o := indexer.Options{Root: root, Extract: noExec}

	runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "1"), o)
	fp1 := metaOf(t, st)
	if res := runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "1"), o); res.Indexed != 0 || res.EdgeLangs != 0 {
		t.Fatalf("same version again: %+v", res)
	}

	res := runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "2"), o)
	if res.Indexed != 2 || res.Unchanged != 0 || res.EdgeLangs != 1 || res.Edges != 4 {
		t.Fatalf("version 2: %+v", res)
	}
	if fp2 := metaOf(t, st); fp2 == fp1 || !isFingerprint(fp2) {
		t.Errorf("fingerprint after the change = %q, before %q", fp2, fp1)
	}
	if res := runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "2"), o); res.Indexed != 0 || res.EdgeLangs != 0 {
		t.Fatalf("version 2 again: %+v", res)
	}
}

func TestLegacyEdgeMetaIsReindexedOnce(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	cfg := fakeCfg("refs", ".ps1")
	runOpts(t, st, cfg, indexer.Options{Root: root})
	if err := st.SetMeta(context.Background(), edgesKey, "1"); err != nil {
		t.Fatal(err)
	}

	if res := runOpts(t, st, cfg, indexer.Options{Root: root}); res.Indexed != 2 || res.EdgeLangs != 1 {
		t.Fatalf("legacy \"1\": %+v", res)
	}
	if fp := metaOf(t, st); !isFingerprint(fp) {
		t.Fatalf("edges meta after the legacy run = %q", fp)
	}
	if res := runOpts(t, st, cfg, indexer.Options{Root: root}); res.Indexed != 0 || res.EdgeLangs != 0 {
		t.Fatalf("after the legacy run: %+v", res)
	}
}

func TestNoEdgesAfterChangeThenEdgesOnly(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	o := indexer.Options{Root: root, Extract: noExec}
	runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "1"), o)

	// the change is fully stale: the files are re-indexed, the edges are left for the next run
	o.NoEdges = true
	if res := runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "2"), o); res.Indexed != 2 || res.EdgeLangs != 0 {
		t.Fatalf("--no-edges after the change: %+v", res)
	}
	if v := metaOf(t, st); v != "" {
		t.Fatalf("edges meta after --no-edges = %q, want empty", v)
	}

	// the next normal run rebuilds the edges without re-indexing any file
	o.NoEdges = false
	res := runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "2"), o)
	if res.Indexed != 0 || res.EdgeLangs != 1 || res.Edges != 4 {
		t.Fatalf("edges-only catch-up: %+v", res)
	}
	if fp := metaOf(t, st); !isFingerprint(fp) {
		t.Fatalf("edges meta after catch-up = %q", fp)
	}
}

func TestFailedStaleRunIsRetriedInFull(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "1"), indexer.Options{Root: root, Extract: noExec})

	// the server does not start during the fully stale run: the old marker must survive
	refused := func(lsp.Spec) (*lsp.Client, error) { return nil, errors.New("server refused to start") }
	res := runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "2"), indexer.Options{Root: root, Extract: noExec, Open: refused})
	if res.Failed != 2 || res.LangErrors["fake"] == "" {
		t.Fatalf("refused server: %+v", res)
	}

	res = runOpts(t, st, withExtractor(fakeCfg("refs", ".ps1"), "2"), indexer.Options{Root: root, Extract: noExec})
	if res.Indexed != 2 || res.EdgeLangs != 1 {
		t.Fatalf("retry after the refused run: %+v", res)
	}
}
