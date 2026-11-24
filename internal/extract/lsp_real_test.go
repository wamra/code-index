package extract

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wamra/code-index/internal/config"
)

// TestRealLSPGopls runs the adapter against a real gopls on a two-file Go
// module. It is skipped unless CODE_INDEX_REAL_LSP=gopls (the value may also be
// the path of the gopls binary).
func TestRealLSPGopls(t *testing.T) {
	bin := os.Getenv("CODE_INDEX_REAL_LSP")
	if bin == "" {
		t.Skip("set CODE_INDEX_REAL_LSP=gopls to run against a real gopls")
	}
	if p, err := exec.LookPath(bin); err == nil {
		bin = p
	} else if h, _ := os.UserHomeDir(); h != "" {
		bin = filepath.Join(h, ".local", "bin", "gopls")
	}
	root, p := writeTree(t, map[string]string{
		"go.mod": "module example.com/x\n\ngo 1.21\n",
		"a.go":   "package x\n\n// Alpha calls Beta.\nfunc Alpha() int {\n\treturn Beta() + 1\n}\n",
		"b.go":   "package x\n\nfunc Beta() int {\n\treturn 2\n}\n\nfunc Gamma() int {\n\treturn Alpha() + Beta()\n}\n",
	})
	a, b := p["a.go"], p["b.go"]
	ex, err := New("go", "gopls", config.Extractor{Type: config.ExtractorLSP, Argv: []string{bin}, LanguageID: "go", StartupTimeoutS: 120, RequestTimeoutS: 60})
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.Start(context.Background(), Env{Root: root, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t.Logf("provides %+v", s.Provides())
	if !s.Provides().Symbols || !s.Provides().Edges {
		t.Fatalf("provides %+v", s.Provides())
	}
	c := symbolsOf(t, s, a, b)
	var known []Record
	for _, f := range []string{a, b} {
		for _, r := range c.recs[f] {
			t.Logf("symbol %+v", r)
		}
		known = append(known, c.recs[f]...)
	}
	e := newCollector(t)
	if err := s.Edges(context.Background(), []string{a, b}, known, e.emit); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range []string{a, b} {
		for _, r := range e.recs[f] {
			t.Logf("edge %+v", r)
			if r.T == TypeCall {
				n++
				if r.Resolution != "exact" || r.Reason != "call-hierarchy" || r.To == "" || r.ToLine == 0 {
					t.Errorf("not an exact call-hierarchy record: %+v", r)
				}
			} else {
				t.Errorf("unexpected record %+v", r)
			}
		}
	}
	// Beta is called in Alpha and Gamma, Alpha in Gamma
	if n != 3 {
		t.Errorf("%d call records, want 3", n)
	}
}
