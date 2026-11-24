package indexer_test

import (
	"context"
	"path/filepath"
	"sort"
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
