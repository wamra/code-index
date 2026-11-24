package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/lsp"
	"github.com/wamra/code-index/internal/lsptest"
)

// The test binary doubles as the fake language server. init runs before
// TestMain, so this needs no change to the TestMain in extract_test.go.
func init() {
	if os.Getenv("CODE_INDEX_FAKE_LSP_SERVE") == "1" {
		lsptest.Main()
		os.Exit(0)
	}
}

const (
	srcA = "# Adds things.\nfunction Alpha {\n  Beta\n  Beta\n}\nclass Box {\n  function Open {\n    Alpha\n  }\n}\n"
	srcB = "function Beta {\n  Alpha\n}\n"
)

func fakeEntry(opts string) config.Extractor {
	return config.Extractor{
		Type:       config.ExtractorLSP,
		Argv:       []string{os.Args[0], "-test.run=^$"},
		Env:        map[string]string{"CODE_INDEX_FAKE_LSP_SERVE": "1", "CODE_INDEX_FAKE_LSP": opts},
		LanguageID: "fake", StartupTimeoutS: 20, RequestTimeoutS: 10,
	}
}

func writeTree(t *testing.T, files map[string]string) (root string, paths map[string]string) {
	t.Helper()
	root = t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	paths = map[string]string{}
	for name, src := range files {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		paths[name] = p
	}
	return root, paths
}

func startFake(t *testing.T, opts, root string, logf func(string, ...any)) Session {
	t.Helper()
	ex, err := New("fake", "fake", fakeEntry(opts))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.Start(context.Background(), Env{Root: root, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// collector is an Emit that keeps what it was given, in order, and fails the
// test when a file is emitted twice or with a foreign record.
type collector struct {
	t     *testing.T
	order []string
	recs  map[string][]Record
}

func newCollector(t *testing.T) *collector { return &collector{t: t, recs: map[string][]Record{}} }

func (c *collector) emit(file string, recs []Record) error {
	c.t.Helper()
	if _, dup := c.recs[file]; dup {
		c.t.Fatalf("%s emitted twice", file)
	}
	for _, r := range recs {
		if r.File != file {
			c.t.Fatalf("record %+v in the emit of %s", r, file)
		}
	}
	c.order = append(c.order, file)
	c.recs[file] = recs
	return nil
}

func ip(i int) *int { return &i }

// wantA is the symbol records of srcA from the fake server with hover: the
// positions are the fixture's own (one-based), derived by hand.
func wantA(file string) []Record {
	return []Record{
		{T: TypeSymbol, File: file, Name: "Alpha", Kind: "function", Line: 2, Col: 1, EndLine: 5, EndCol: 2, SelLine: 2, SelCol: 10,
			Qualified: "Alpha", Signature: "function Alpha", Doc: "Adds things."},
		{T: TypeSymbol, File: file, Name: "Box", Kind: "class", Line: 6, Col: 1, EndLine: 10, EndCol: 2, SelLine: 6, SelCol: 7,
			Qualified: "Box", Signature: "function Box"},
		{T: TypeSymbol, File: file, Name: "Open", Kind: "function", Line: 7, Col: 1, EndLine: 9, EndCol: 4, SelLine: 7, SelCol: 12,
			Qualified: "Box.Open", Signature: "function Open", Parent: ip(1)},
	}
}

func symbolsOf(t *testing.T, s Session, files ...string) *collector {
	t.Helper()
	c := newCollector(t)
	if err := s.Symbols(context.Background(), files, c.emit); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLSPSymbolsHierarchicalAndFlatWithHover(t *testing.T) {
	for _, opts := range []string{"", "flat"} {
		t.Run("opts="+opts, func(t *testing.T) {
			root, p := writeTree(t, map[string]string{"a.ps1": srcA})
			c := symbolsOf(t, startFake(t, opts, root, nil), p["a.ps1"])
			if got, want := c.recs[p["a.ps1"]], wantA(p["a.ps1"]); !reflect.DeepEqual(got, want) {
				t.Errorf("records\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestLSPSymbolsWithoutHoverCarryDetailOnly(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA})
	c := symbolsOf(t, startFake(t, "nohover", root, nil), p["a.ps1"])
	for _, r := range c.recs[p["a.ps1"]] {
		if r.Signature != "" || r.Doc != "" {
			t.Errorf("%s: signature/doc without hover: %+v", r.Name, r)
		}
	}
}

func TestLSPSymbolsCRLFAndBOMGiveTheRecordsOfLF(t *testing.T) {
	lf := srcA
	crlf := "\ufeff" + strings.ReplaceAll(srcA, "\n", "\r\n")
	got := map[string][]Record{}
	for name, src := range map[string]string{"lf": lf, "crlf+bom": crlf} {
		root, p := writeTree(t, map[string]string{"a.ps1": src})
		recs := symbolsOf(t, startFake(t, "", root, nil), p["a.ps1"]).recs[p["a.ps1"]]
		for i := range recs {
			recs[i].File = "a.ps1"
			// the fake server counts the "\r" of a CRLF line in the end column of
			// a range that ends at the end of the line; nothing else differs
			recs[i].EndCol = 0
		}
		got[name] = recs
	}
	if len(got["lf"]) != 3 || !reflect.DeepEqual(got["lf"], got["crlf+bom"]) {
		t.Errorf("lf    %+v\ncrlf  %+v", got["lf"], got["crlf+bom"])
	}
}

func TestLSPSymbolsEmptyAnswerRetry(t *testing.T) {
	old := EmptyRetryDelays
	defer func() { EmptyRetryDelays = old }()

	root, p := writeTree(t, map[string]string{"a.ps1": srcA})
	EmptyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	c := symbolsOf(t, startFake(t, "emptyfirst=2", root, nil), p["a.ps1"])
	if n := len(c.recs[p["a.ps1"]]); n != 3 {
		t.Errorf("with retries: %d records, want 3", n)
	}
	EmptyRetryDelays = nil
	c = symbolsOf(t, startFake(t, "emptyfirst=1", root, nil), p["a.ps1"])
	if recs, ok := c.recs[p["a.ps1"]]; !ok || len(recs) != 0 {
		t.Errorf("without retries: %+v (emitted %v), want an empty emit", recs, ok)
	}
}

func TestLSPRetryStateStopsAfterThreeWastedRetries(t *testing.T) {
	rs := &retryState{}
	if !rs.allow() {
		t.Fatal("a fresh state must allow retries")
	}
	rs.wasted = 3
	if rs.allow() {
		t.Error("three wasted retries must stop them")
	}
	rs.helped = 1
	if !rs.allow() {
		t.Error("a retry that helped keeps them on")
	}
}

func TestLSPSymbolsPerFileFailureIsAnErrorRecord(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB, "c.ps1": srcB})
	a, b, cc := p["a.ps1"], p["b.ps1"], p["c.ps1"]
	c := symbolsOf(t, startFake(t, "failpath=b.ps1", root, nil), a, b, cc)
	if !reflect.DeepEqual(c.order, []string{a, b, cc}) {
		t.Fatalf("emit order %v", c.order)
	}
	if len(c.recs[a]) != 3 || len(c.recs[cc]) != 1 {
		t.Errorf("other files must still emit: a=%d c=%d records", len(c.recs[a]), len(c.recs[cc]))
	}
	if got := c.recs[b]; len(got) != 1 || got[0].T != TypeError || !strings.Contains(got[0].Message, "internal error in fake server") {
		t.Errorf("b: %+v", got)
	}
}

func TestLSPSymbolsUnreadableFileIsAnErrorRecord(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA})
	missing := filepath.Join(root, "gone.ps1")
	c := symbolsOf(t, startFake(t, "", root, nil), missing, p["a.ps1"])
	if got := c.recs[missing]; len(got) != 1 || got[0].T != TypeError || got[0].Message == "" {
		t.Errorf("missing file: %+v", got)
	}
	if len(c.recs[p["a.ps1"]]) != 3 {
		t.Errorf("a.ps1: %+v", c.recs[p["a.ps1"]])
	}
}

func TestLSPSymbolsServerDeathIsTheReturnedError(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB, "c.ps1": srcB})
	c := newCollector(t)
	s := startFake(t, "diepath=b.ps1", root, nil)
	err := s.Symbols(context.Background(), []string{p["a.ps1"], p["b.ps1"], p["c.ps1"]}, c.emit)
	if !errors.Is(err, lsp.ErrClosed) {
		t.Fatalf("err = %v, want lsp.ErrClosed", err)
	}
	if !reflect.DeepEqual(c.order, []string{p["a.ps1"]}) {
		t.Errorf("emitted %v, want only a.ps1 (before the death)", c.order)
	}
}

func TestLSPSymbolsStopOnEmitError(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	stop := errors.New("store failed")
	calls := 0
	err := startFake(t, "", root, nil).Symbols(context.Background(), []string{p["a.ps1"], p["b.ps1"]}, func(string, []Record) error {
		calls++
		return stop
	})
	if err != stop || calls != 1 {
		t.Errorf("err=%v calls=%d, want the emit error after one call", err, calls)
	}
}

func TestLSPSymbolsCancelledContext(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := startFake(t, "", root, nil).Symbols(ctx, []string{p["a.ps1"]}, newCollector(t).emit)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func TestLSPProvides(t *testing.T) {
	cases := []struct {
		opts string
		want Provides
	}{
		{"", Provides{Symbols: true}},
		{"refs", Provides{Symbols: true, Edges: true}},
		{"callhier", Provides{Symbols: true, Edges: true}},
		{"callhier,refs", Provides{Symbols: true, Edges: true}},
		{"nodocsymbol", Provides{}},
		{"nodocsymbol,refs", Provides{Edges: true}},
		{"nodocsymbol,callhier", Provides{Edges: true}},
	}
	for _, c := range cases {
		root, _ := writeTree(t, nil)
		if got := startFake(t, c.opts, root, nil).Provides(); got != c.want {
			t.Errorf("%q: Provides = %+v, want %+v", c.opts, got, c.want)
		}
	}
}

// knownFrom builds the known argument the way the indexer does: the symbol
// records of every file.
func knownFrom(t *testing.T, s Session, files ...string) []Record {
	t.Helper()
	c := symbolsOf(t, s, files...)
	var known []Record
	for _, f := range files {
		known = append(known, c.recs[f]...)
	}
	return known
}

func call(file string, line, col int, name, to string, toLine, toCol int, res, reason string) Record {
	return Record{T: TypeCall, File: file, Line: line, Col: col, Name: name, To: to, ToLine: toLine, ToCol: toCol, Resolution: res, Reason: reason}
}

// wantCalls are the call sites of the fixture (srcA in a.ps1, srcB in b.ps1)
// the fake server finds: Beta is called twice in Alpha, Alpha from Open and
// from Beta, nobody calls Open.
func wantCalls(a, b, res, reason string) map[string][]Record {
	return map[string][]Record{
		a: {
			call(a, 3, 3, "Beta", b, 1, 10, res, reason),
			call(a, 4, 3, "Beta", b, 1, 10, res, reason),
			call(a, 8, 5, "Alpha", a, 2, 10, res, reason),
		},
		b: {call(b, 2, 3, "Alpha", a, 2, 10, res, reason)},
	}
}

func TestLSPEdgesCallHierarchyIsExact(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	a, b := p["a.ps1"], p["b.ps1"]
	s := startFake(t, "callhier", root, nil)
	c := newCollector(t)
	if err := s.Edges(context.Background(), []string{a, b}, knownFrom(t, s, a, b), c.emit); err != nil {
		t.Fatal(err)
	}
	if want := wantCalls(a, b, "exact", "call-hierarchy"); !reflect.DeepEqual(c.recs, want) {
		t.Errorf("records\n got %+v\nwant %+v", c.recs, want)
	}
}

func TestLSPEdgesReferencesAreCandidatesAndSkipTheDeclaration(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	a, b := p["a.ps1"], p["b.ps1"]
	s := startFake(t, "refs", root, nil)
	c := newCollector(t)
	if err := s.Edges(context.Background(), []string{a, b}, knownFrom(t, s, a, b), c.emit); err != nil {
		t.Fatal(err)
	}
	if want := wantCalls(a, b, "candidate", "references"); !reflect.DeepEqual(c.recs, want) {
		t.Errorf("records\n got %+v\nwant %+v", c.recs, want)
	}
	for _, r := range c.recs[a] {
		if r.Line == 2 {
			t.Errorf("the declaration of Alpha was reported as a call: %+v", r)
		}
	}
}

func TestLSPEdgesPreferCallHierarchyWhenBothAreAdvertised(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	a, b := p["a.ps1"], p["b.ps1"]
	s := startFake(t, "callhier,refs", root, nil)
	c := newCollector(t)
	if err := s.Edges(context.Background(), []string{a, b}, knownFrom(t, s, a, b), c.emit); err != nil {
		t.Fatal(err)
	}
	if want := wantCalls(a, b, "exact", "call-hierarchy"); !reflect.DeepEqual(c.recs, want) {
		t.Errorf("records\n got %+v\nwant %+v", c.recs, want)
	}
}

func TestLSPEdgesEmitEveryRequestedFileOnceAndDropOthers(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB, "c.ps1": "# nothing here\n"})
	a, b, cf := p["a.ps1"], p["b.ps1"], p["c.ps1"]
	s := startFake(t, "callhier", root, nil)
	s.(*lspSession).delays = nil // c.ps1 has no symbols; do not wait for the empty-answer retry
	known := knownFrom(t, s, a, b, cf)
	c := newCollector(t)
	// b.ps1 is known (its Beta is a target) but not requested: its call site
	// of Alpha is dropped, and Beta's callers in a.ps1 stay.
	if err := s.Edges(context.Background(), []string{cf, a}, known, c.emit); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.order, []string{cf, a}) {
		t.Fatalf("emit order %v, want [c a]", c.order)
	}
	if len(c.recs[cf]) != 0 {
		t.Errorf("c.ps1: %+v", c.recs[cf])
	}
	if want := wantCalls(a, b, "exact", "call-hierarchy")[a]; !reflect.DeepEqual(c.recs[a], want) {
		t.Errorf("a.ps1\n got %+v\nwant %+v", c.recs[a], want)
	}
}

func TestLSPEdgesFailingRequestsAreCountedAndReported(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	a, b := p["a.ps1"], p["b.ps1"]
	for _, opts := range []string{"callhierfail", "refsfail"} {
		var logs []string
		logf := func(f string, args ...any) { logs = append(logs, strings.TrimSpace(fmt.Sprintf(f, args...))) }
		s := startFake(t, opts, root, logf)
		known := knownFrom(t, s, a, b)
		c := newCollector(t)
		if err := s.Edges(context.Background(), []string{a, b}, known, c.emit); err != nil {
			t.Fatalf("%s: a failing request must not fail the batch: %v", opts, err)
		}
		if len(c.order) != 2 {
			t.Errorf("%s: emitted %v", opts, c.order)
		}
		// 3 callable targets (Alpha, Open, Beta), all failing; the first is in a.ps1
		got := c.recs[a]
		if len(got) != 1 || got[0].T != TypeError || !strings.Contains(got[0].Message, "3 of 3") {
			t.Errorf("%s: a.ps1 records %+v", opts, got)
		}
		if len(c.recs[b]) != 0 {
			t.Errorf("%s: b.ps1 records %+v", opts, c.recs[b])
		}
		if !strings.Contains(strings.Join(logs, "\n"), "3 of 3") {
			t.Errorf("%s: logs %q", opts, logs)
		}
	}
}

func TestLSPEdgesServerDeathIsTheReturnedError(t *testing.T) {
	root, p := writeTree(t, map[string]string{"a.ps1": srcA, "b.ps1": srcB})
	a, b := p["a.ps1"], p["b.ps1"]
	s := startFake(t, "callhier,diepath=b.ps1", root, nil)
	// the server dies on the documentSymbol of b.ps1, so give Edges known from a
	// session of a healthy server
	known := knownFrom(t, startFake(t, "callhier", root, nil), a, b)
	c := newCollector(t)
	err := s.Edges(context.Background(), []string{a, b}, known, c.emit)
	if !errors.Is(err, lsp.ErrClosed) {
		t.Fatalf("err = %v, want lsp.ErrClosed", err)
	}
}

func TestLSPVersionDoesNotStartTheServer(t *testing.T) {
	ec := config.Extractor{Type: config.ExtractorLSP, Argv: []string{"definitely-not-installed-lsp"}, Version: "9.1"}
	ex, err := New("fake", "x", ec)
	if err != nil {
		t.Fatal(err)
	}
	if v := ex.Version(context.Background()); v != "9.1" {
		t.Errorf("Version = %q", v)
	}
	if ex.Name() != "x" || ex.Type() != config.ExtractorLSP || !reflect.DeepEqual(ex.Argv(), ec.Argv) {
		t.Errorf("identity: %q %q %v", ex.Name(), ex.Type(), ex.Argv())
	}
}

func TestLSPStartMissingCommandIsUnavailable(t *testing.T) {
	t.Setenv("CODE_INDEX_TEST_LSP_CMD", "")
	cases := map[string]config.Extractor{
		"missing":              {Type: config.ExtractorLSP, Argv: []string{"definitely-not-installed-lsp"}},
		"missing + fallback":   {Type: config.ExtractorLSP, Argv: []string{"definitely-not-installed-lsp"}, CommandFallbacks: []string{"also-not-installed-lsp"}},
		"command_env, no fall": {Type: config.ExtractorLSP, Argv: []string{os.Args[0]}, CommandEnv: "CODE_INDEX_TEST_LSP_BAD", CommandFallbacks: []string{os.Args[0]}},
	}
	t.Setenv("CODE_INDEX_TEST_LSP_BAD", "definitely-not-installed-lsp")
	for name, ec := range cases {
		ex, err := New("fake", "x", ec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ex.Start(context.Background(), Env{Root: t.TempDir()}); !errors.Is(err, ErrExtractorUnavailable) {
			t.Errorf("%s: err = %v, want ErrExtractorUnavailable", name, err)
		}
	}
}

func TestLSPStartResolvesFallbackAndCommandEnv(t *testing.T) {
	root, _ := writeTree(t, nil)
	base := fakeEntry("")
	cases := map[string]config.Extractor{
		"fallback": func() config.Extractor {
			ec := base
			ec.Argv = []string{"definitely-not-installed-lsp", "-test.run=^$"}
			ec.CommandFallbacks = []string{os.Args[0]}
			return ec
		}(),
		"command_env": func() config.Extractor {
			ec := base
			ec.Argv = []string{"definitely-not-installed-lsp", "-test.run=^$"}
			ec.CommandEnv = "CODE_INDEX_TEST_LSP_CMD"
			return ec
		}(),
	}
	t.Setenv("CODE_INDEX_TEST_LSP_CMD", os.Args[0])
	for name, ec := range cases {
		ex, _ := New("fake", "x", ec)
		s, err := ex.Start(context.Background(), Env{Root: root})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !s.Provides().Symbols {
			t.Errorf("%s: no symbols", name)
		}
		s.Close()
	}
}

func TestLSPStartUsesOpenLSPWithExpandedSpec(t *testing.T) {
	root, _ := writeTree(t, nil)
	ec := fakeEntry("")
	ec.Argv = append(ec.Argv, "--root={root}")
	ex, _ := New("fake", "x", ec)
	var got lsp.Spec
	opens := 0
	s, err := ex.Start(context.Background(), Env{Root: root, OpenLSP: func(sp lsp.Spec) (*lsp.Client, error) {
		opens++
		got = sp
		return lsp.Start(sp)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if opens != 1 || got.Dir != root || got.Args[len(got.Args)-1] != "--root="+root || got.StartupTimeout != 20*time.Second || got.RequestTimeout != 10*time.Second {
		t.Errorf("opens=%d spec=%+v", opens, got)
	}
	if !contains(got.Env, "CODE_INDEX_FAKE_LSP_SERVE=1") {
		t.Errorf("env %v", got.Env)
	}
}

func TestLSPStartRejectsAnUnknownPlaceholder(t *testing.T) {
	ec := fakeEntry("")
	ec.Argv = append(ec.Argv, "{nope}")
	ex, _ := New("fake", "x", ec)
	if _, err := ex.Start(context.Background(), Env{Root: t.TempDir()}); err == nil || errors.Is(err, ErrExtractorUnavailable) {
		t.Errorf("err = %v", err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
