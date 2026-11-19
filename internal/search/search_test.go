package search_test

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
	"github.com/wamra/code-index/internal/embed"
	"github.com/wamra/code-index/internal/embedtest"
	"github.com/wamra/code-index/internal/indexer"
	"github.com/wamra/code-index/internal/lsptest"
	"github.com/wamra/code-index/internal/provider"
	"github.com/wamra/code-index/internal/search"
	"github.com/wamra/code-index/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("CODE_INDEX_FAKE_LSP_SERVE") == "1" {
		lsptest.Main()
		return
	}
	os.Exit(m.Run())
}

// workspace is an indexed copy of the PowerShell fixture plus a shell file.
type workspace struct {
	root string
	st   *store.Store
}

func setup(t *testing.T) *workspace {
	t.Helper()
	root := t.TempDir()
	src := "../../testdata/fixture-ps"
	filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(root, rel), 0o755)
		}
		b, _ := os.ReadFile(p)
		return os.WriteFile(filepath.Join(root, rel), b, 0o644)
	})
	os.WriteFile(filepath.Join(root, "deploy.sh"), []byte("# Uploads the build.\ndeploy_release() {\n}\n"), 0o644)
	root, _ = filepath.EvalSymlinks(root)
	st, err := store.Open(filepath.Join(t.TempDir(), "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{Languages: map[string]config.Language{
		"powershell": {Command: os.Args[0], Args: []string{"-test.run=^$"}, Env: map[string]string{"CODE_INDEX_FAKE_LSP_SERVE": "1"}, Extensions: []string{".ps1", ".psm1", ".psd1"}, StartupTimeoutS: 20, RequestTimeoutS: 10},
		"bash":       {Command: os.Args[0], Args: []string{"-test.run=^$"}, Env: map[string]string{"CODE_INDEX_FAKE_LSP_SERVE": "1"}, Extensions: []string{".sh"}, StartupTimeoutS: 20, RequestTimeoutS: 10},
	}}.Resolved()
	if _, err := indexer.Run(context.Background(), st, cfg, indexer.Options{Root: root, Log: io.Discard, EmptyRetryDelays: []time.Duration{time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	return &workspace{root, st}
}

func names(hits []search.Hit) string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Qualified)
	}
	return strings.Join(out, ",")
}

func run(t *testing.T, w *workspace, q string, o search.Opts) ([]search.Hit, string) {
	t.Helper()
	o.Root = w.root
	hits, route, err := search.Run(context.Background(), w.st, q, o)
	if err != nil {
		t.Fatal(err)
	}
	return hits, route
}

func TestIdentifierQueryRanksDefinitionFirst(t *testing.T) {
	w := setup(t)
	hits, route := run(t, w, "Get-Widget", search.Opts{})
	if route != "name" || len(hits) == 0 {
		t.Fatalf("route=%s hits=%v", route, names(hits))
	}
	h := hits[0]
	if h.Qualified != "Get-Widget" || h.Kind != "function" || h.Path != "Widgets/Widgets.psm1" || h.Line != 6 || h.Signature != "function Get-Widget" {
		t.Errorf("top hit = %+v", h)
	}
	// substring and case-insensitive
	hits, _ = run(t, w, "widget", search.Opts{})
	if !strings.Contains(names(hits), "Get-Widget") || !strings.Contains(names(hits), "New-Widget") {
		t.Errorf("widget -> %s", names(hits))
	}
	// a path-shaped query finds the symbols of that file
	hits, _ = run(t, w, "Widgets/Widgets.psm1", search.Opts{})
	if len(hits) < 2 || hits[0].Path != "Widgets/Widgets.psm1" {
		t.Errorf("path query -> %s", names(hits))
	}
	// too short for trigram: no name hits, falls through to full text without error
	if _, _, err := search.Run(context.Background(), w.st, "gw", search.Opts{}); err != nil {
		t.Errorf("short query: %v", err)
	}
}

func TestPlainLanguageFallsBackToFullText(t *testing.T) {
	w := setup(t)
	hits, route := run(t, w, "function that returns a widget by name", search.Opts{K: 3})
	if route != "fts" || len(hits) == 0 || hits[0].Qualified != "Get-Widget" {
		t.Fatalf("route=%s hits=%s", route, names(hits))
	}
	if len(hits) > 3 {
		t.Errorf("k not honored: %d hits", len(hits))
	}
}

func TestFilters(t *testing.T) {
	w := setup(t)
	hits, _ := run(t, w, "widget", search.Opts{Kind: "function", Lang: "powershell", K: 20})
	for _, h := range hits {
		if h.Kind != "function" || h.Lang != "powershell" {
			t.Errorf("filter leak: %+v", h)
		}
	}
	hits, _ = run(t, w, "widget", search.Opts{Path: "Widgets/", K: 20})
	if len(hits) == 0 {
		t.Fatal("no hits under Widgets/")
	}
	for _, h := range hits {
		if !strings.HasPrefix(h.Path, "Widgets/") {
			t.Errorf("path filter leak: %+v", h)
		}
	}
	hits, _ = run(t, w, "deploy_release", search.Opts{Lang: "powershell"})
	if len(hits) != 0 {
		t.Errorf("lang filter should exclude the bash symbol: %s", names(hits))
	}
	hits, _ = run(t, w, "Widgets", search.Opts{Mode: "fts", Kind: "file", K: 20})
	if len(hits) == 0 || hits[0].Kind != "file" || hits[0].TargetKind != "file" {
		t.Errorf("kind=file: %+v", hits)
	}
}

func TestStaleMarker(t *testing.T) {
	w := setup(t)
	os.WriteFile(filepath.Join(w.root, "lib.ps1"), []byte("function Invoke-Helper {\n  # changed\n}\n"), 0o644)
	os.Remove(filepath.Join(w.root, "deploy.sh"))
	hits, _ := run(t, w, "Invoke-Helper", search.Opts{})
	if len(hits) == 0 || hits[0].Stale != "modified" {
		t.Errorf("modified file: %+v", hits)
	}
	hits, _ = run(t, w, "deploy_release", search.Opts{})
	if len(hits) == 0 || hits[0].Stale != "deleted" {
		t.Errorf("deleted file: %+v", hits)
	}
	hits, _ = run(t, w, "Get-Widget", search.Opts{})
	if hits[0].Stale != "" {
		t.Errorf("untouched file marked stale: %+v", hits[0])
	}
}

func embedder(t *testing.T, url, model string) provider.Embedder {
	e, err := provider.NewEmbedder(provider.EmbedConfig{Provider: provider.Ollama, BaseURL: url, Model: model, Dims: 64,
		DocPrefix: provider.Str(""), QueryPrefix: provider.Str("")})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestVectorRoutingWithEmbedder(t *testing.T) {
	srv := embedtest.New(64, "fake", "other")
	defer srv.Close()
	w := setup(t)
	e := embedder(t, srv.URL, "fake")
	ctx := context.Background()

	var warnings []string
	opts := search.Opts{Embedder: e, Warn: func(m string) { warnings = append(warnings, m) }}

	// configured but nothing embedded yet: auto falls back to full text and says why
	hits, route := run(t, w, "returns a widget by name", opts)
	if route != "fts" || len(hits) == 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "no embeddings stored yet") {
		t.Fatalf("before embedding: route=%s warnings=%v", route, warnings)
	}
	if _, _, err := search.Run(ctx, w.st, "x", search.Opts{Mode: "vector", Embedder: e}); !errors.Is(err, search.ErrNoVectors) {
		t.Errorf("vector before embedding: %v", err)
	}
	if _, _, err := search.Run(ctx, w.st, "x", search.Opts{Mode: "vector"}); !errors.Is(err, search.ErrNoVectors) {
		t.Errorf("vector without embedder: %v", err)
	}

	if n, err := embed.Drain(ctx, w.st, e, embed.Opts{}); err != nil || n == 0 {
		t.Fatalf("drain: n=%d err=%v", n, err)
	}

	warnings = nil
	hits, route = run(t, w, "returns a widget by name", opts)
	if route != "vector" || len(hits) == 0 || hits[0].Qualified != "Get-Widget" || len(warnings) != 0 {
		t.Fatalf("auto after embedding: route=%s hits=%s warnings=%v", route, names(hits), warnings)
	}
	// an identifier still goes to the name index
	if _, route = run(t, w, "Get-Widget", opts); route != "name" {
		t.Errorf("identifier route = %s", route)
	}
	// single bare word that matches no name falls through to the vector index
	if _, route = run(t, w, "uploads", opts); route != "vector" {
		t.Errorf("fall-through route = %s", route)
	}
	// filters apply inside the KNN
	hits, _ = run(t, w, "returns a widget by name", search.Opts{Mode: "vector", Embedder: e, Kind: "function", Lang: "bash", K: 20})
	for _, h := range hits {
		if h.Kind != "function" || h.Lang != "bash" {
			t.Errorf("vector filter leak: %+v", h)
		}
	}
	if len(hits) != 1 || hits[0].Qualified != "deploy_release" {
		t.Errorf("vector kind+lang filter -> %s", names(hits))
	}
	hits, _ = run(t, w, "returns a widget by name", search.Opts{Mode: "vector", Embedder: e, Path: "Widgets/", K: 20})
	if len(hits) == 0 || !strings.HasPrefix(hits[0].Path, "Widgets/") {
		t.Errorf("vector path filter -> %+v", hits)
	}

	hits, route = run(t, w, "returns a widget by name", search.Opts{Mode: "hybrid", Embedder: e})
	if route != "hybrid (fts+vector)" || len(hits) == 0 || hits[0].Qualified != "Get-Widget" {
		t.Errorf("hybrid: route=%s hits=%s", route, names(hits))
	}
	hits, route = run(t, w, "returns a widget by name", search.Opts{Mode: "fts", Embedder: e})
	if route != "fts" || len(hits) == 0 {
		t.Errorf("fts mode: route=%s", route)
	}

	// provider down at query time: auto degrades to full text with a warning, vector fails
	srv.Down.Store(true)
	warnings = nil
	hits, route = run(t, w, "returns a widget by name", opts)
	if route != "fts" || len(hits) == 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "unreachable") {
		t.Errorf("down: route=%s warnings=%v", route, warnings)
	}
	if _, _, err := search.Run(ctx, w.st, "x", search.Opts{Mode: "vector", Embedder: e}); !errors.Is(err, provider.ErrDown) {
		t.Errorf("vector while down: %v", err)
	}
	srv.Down.Store(false)

	// a different embedder: vectors of another model must not be searched
	warnings = nil
	other := embedder(t, srv.URL, "other")
	_, route = run(t, w, "returns a widget by name", search.Opts{Embedder: other, Warn: opts.Warn})
	if route != "fts" || len(warnings) != 1 || !strings.Contains(warnings[0], "re-embed") {
		t.Errorf("fingerprint mismatch: route=%s warnings=%v", route, warnings)
	}
}

func TestEmptyAndUnknownMode(t *testing.T) {
	w := setup(t)
	if _, _, err := search.Run(context.Background(), w.st, "  ", search.Opts{}); err == nil {
		t.Error("empty query must fail")
	}
	if _, _, err := search.Run(context.Background(), w.st, "x", search.Opts{Mode: "nope"}); err == nil {
		t.Error("unknown mode must fail")
	}
}
