package indexer_test

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/indexer"
)

// Each exec extractor is given the files of the language that its "extensions"
// select (all of the language's when it lists none), and runs in config order.
func TestExecExtractorsGetTheirExtensions(t *testing.T) {
	root := copyTree(t, fixturePS)
	st := openStore(t)
	cfg := fakeCfg("refs", psExts...)
	cfg.Extractors = map[string]config.Extractor{
		"only-modules": {Type: config.ExtractorExec, Argv: []string{"unused"}, Extensions: []string{".PSM1"}},
		"everything":   {Type: config.ExtractorExec, Argv: []string{"unused"}},
	}
	l := cfg.Languages["fake"]
	l.Extractor = config.NewExtractorNames("only-modules", "everything")
	cfg.Languages["fake"] = l
	if err := cfg.Resolved().Validate(); err != nil {
		t.Fatal(err)
	}
	var order []string
	seen := map[string][]string{}
	extract := func(_ context.Context, name string, files []string) ([]byte, error) {
		order = append(order, name)
		for _, f := range files {
			rel, _ := filepath.Rel(root, f)
			seen[name] = append(seen[name], filepath.ToSlash(rel))
		}
		sort.Strings(seen[name])
		return nil, nil
	}
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: extract})
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v", res.Notes)
	}
	if got := strings.Join(order, ","); got != "only-modules,everything" {
		t.Errorf("run order = %s", got)
	}
	if got := strings.Join(seen["only-modules"], ","); got != "Widgets/Widgets.psm1" {
		t.Errorf("only-modules got %s", got)
	}
	if got := strings.Join(seen["everything"], ","); got != "Widgets/Widgets.psd1,Widgets/Widgets.psm1,bad.ps1,lib.ps1,main.ps1" {
		t.Errorf("everything got %s", got)
	}
}

// An extractor that prints only a usage message and exits 0 is a failure: its
// files are failed (and retried), and the language's edges are not marked
// computed.
func TestGarbageOnlyExtractorOutputFailsFilesAndIsRetried(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	cfg := withExtractor(fakeCfg("refs", ".ps1"), "1")
	yes := true
	e := cfg.Extractors["ext"]
	e.Symbols = &yes
	cfg.Extractors["ext"] = e
	l := cfg.Languages["fake"] // exec only: no language server
	l.Command, l.Args, l.Env = "", nil, nil
	cfg.Languages["fake"] = l
	cfg = cfg.Resolved()
	garbage := func(context.Context, string, []string) ([]byte, error) {
		return []byte("usage: ext [-h] --list FILE\n"), nil
	}
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: garbage})
	if res.Failed != 2 || res.Indexed != 0 {
		t.Fatalf("garbage run: indexed=%d failed=%d errors=%v", res.Indexed, res.Failed, res.FileErrors)
	}
	if !strings.Contains(res.LangErrors["fake"], "usage: ext [-h] --list FILE") {
		t.Errorf("language error = %q", res.LangErrors["fake"])
	}
	if fp := metaOf(t, st); fp != "" {
		t.Errorf("edges fingerprint must not be set after a failed run, got %q", fp)
	}
	good := func(_ context.Context, _ string, files []string) ([]byte, error) {
		var b strings.Builder
		for _, f := range files {
			b.WriteString(`{"t":"func","file":` + strconv.Quote(f) + `,"name":"F","line":1,"col":1}` + "\n")
		}
		return []byte(b.String()), nil
	}
	res = runOpts(t, st, cfg, indexer.Options{Root: root, Extract: good})
	if res.Indexed != 2 || res.Failed != 0 {
		t.Fatalf("retry run: indexed=%d failed=%d errors=%v", res.Indexed, res.Failed, res.FileErrors)
	}
	if fp := metaOf(t, st); !isFingerprint(fp) {
		t.Errorf("edges fingerprint after a good run = %q", fp)
	}
}

// The same rule in phase 2: an edges-only extractor that prints only garbage
// leaves the language's edges not computed, so the next run tries again.
func TestGarbageOnlyEdgesOutputLeavesEdgesNotComputed(t *testing.T) {
	root := tree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	st := openStore(t)
	cfg := withExtractor(fakeCfg("refs", ".ps1"), "1") // the language server gives symbols, "ext" edges
	garbage := func(context.Context, string, []string) ([]byte, error) { return []byte("panic: boom\n"), nil }
	res := runOpts(t, st, cfg, indexer.Options{Root: root, Extract: garbage})
	if fp := metaOf(t, st); fp != "" {
		t.Errorf("edges fingerprint set after garbage edges output: %q (result %+v)", fp, res)
	}
	if !strings.Contains(res.LangErrors["fake"], "panic: boom") && !strings.Contains(strings.Join(res.Notes, "\n"), "panic: boom") {
		t.Errorf("the garbage line is not reported: errors=%v notes=%v", res.LangErrors, res.Notes)
	}
	empty := func(context.Context, string, []string) ([]byte, error) { return nil, nil }
	runOpts(t, st, cfg, indexer.Options{Root: root, Extract: empty})
	if fp := metaOf(t, st); !isFingerprint(fp) {
		t.Errorf("fingerprint after a good run = %q", fp)
	}
}
