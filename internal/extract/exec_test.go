package extract

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/config"
)

// helperMain is the fake extractor: the test binary run with
// EXTRACT_TEST_HELPER=<mode>. It reads the list file named by the first
// argument and prints one "func" record per listed file (plus the mode's quirk).
func helperMain() {
	mode := os.Getenv("EXTRACT_TEST_HELPER")
	args := os.Args[1:]
	if mode == "sleep" {
		time.Sleep(time.Minute)
		return
	}
	if mode == "spawn" { // start a long-lived grandchild that shares our stdout/stderr, record its pid
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), "EXTRACT_TEST_HELPER=sleep")
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn:", err)
			os.Exit(4)
		}
		os.WriteFile(os.Getenv("EXTRACT_TEST_PIDFILE"), []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		time.Sleep(time.Minute)
		return
	}
	if mode == "garbage" { // a usage message on stdout, exit 0
		fmt.Println("usage: extractor [-h] --list FILE")
		fmt.Println("second line")
		return
	}
	if mode == "silent" { // nothing at all, exit 0
		return
	}
	list := ""
	for i, a := range args {
		if a == "--list" && i+1 < len(args) {
			list = args[i+1]
		}
	}
	f, err := os.Open(list)
	if err != nil {
		fmt.Fprintln(os.Stderr, "no list file:", err)
		os.Exit(3)
	}
	defer f.Close()
	if mode == "dump" { // print the raw list file and the arguments
		b, _ := os.ReadFile(list)
		out, _ := json.Marshal(map[string]any{"t": "func", "file": "dump", "name": string(b), "in": strings.Join(args, "|")})
		wd, _ := os.Getwd()
		out2, _ := json.Marshal(map[string]any{"t": "func", "file": "dump", "name": os.Getenv("EXTRACT_TEST_EXTRA"), "in": wd})
		fmt.Println(string(out))
		fmt.Println(string(out2))
		return
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		path := sc.Text()
		if strings.HasSuffix(path, "empty.x") {
			continue // no records for this file
		}
		out, _ := json.Marshal(map[string]any{"t": "func", "file": path, "name": "F", "line": 1, "col": 1})
		fmt.Println(string(out))
	}
	switch mode {
	case "noise":
		fmt.Println("WARNING: noise")
	case "stray":
		out, _ := json.Marshal(map[string]any{"t": "func", "file": "/nowhere/else.x", "name": "G", "line": 1, "col": 1})
		fmt.Println(string(out))
	case "fail":
		fmt.Fprint(os.Stderr, strings.Repeat("e", 500)+"TAIL")
		os.Exit(7)
	}
}

func helperEC(mode string, extra ...string) config.Extractor {
	return config.Extractor{
		Type: config.ExtractorExec, TimeoutS: 30,
		Argv: append([]string{os.Args[0], "-test.run=^$", "--list", "{listFile}"}, extra...),
		Env:  map[string]string{"EXTRACT_TEST_HELPER": mode},
	}
}

type got map[string][]Record

func runEdges(t *testing.T, ec config.Extractor, env Env, files []string) (got, Session, error) {
	t.Helper()
	ex, err := New("lang", "fake", ec)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ex.Start(context.Background(), env)
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { s.Close() })
	g := got{}
	err = s.Edges(context.Background(), files, nil, func(f string, rs []Record) error {
		if _, dup := g[f]; dup {
			t.Errorf("%s emitted twice", f)
		}
		g[f] = rs
		return nil
	})
	return g, s, err
}

func TestExecEmitsOncePerRequestedFile(t *testing.T) {
	root := t.TempDir()
	files := []string{filepath.Join(root, "a.x"), filepath.Join(root, "empty.x"), filepath.Join(root, "b.x")}
	var logs []string
	env := Env{Root: root, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	g, s, err := runEdges(t, helperEC("stray"), env, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		recs, ok := g[f]
		if !ok {
			t.Errorf("%s was not emitted", f)
		}
		want := 1
		if strings.HasSuffix(f, "empty.x") {
			want = 0
		}
		if len(recs) != want {
			t.Errorf("%s: %d record(s), want %d", f, len(recs), want)
		}
		for _, r := range recs {
			if r.File != f {
				t.Errorf("record file %q != %q", r.File, f)
			}
		}
	}
	if len(g) != 3 {
		t.Errorf("emitted %d files: %v", len(g), g)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "ignored 1 record(s) for files that were not requested") {
		t.Errorf("logs = %q", logs)
	}
	if n := s.(Noter).Notes(); len(n) != 0 {
		t.Errorf("notes = %q", n)
	}
}

func TestExecNonJSONLinesAreNoted(t *testing.T) {
	root := t.TempDir()
	_, s, err := runEdges(t, helperEC("noise"), Env{Root: root}, []string{filepath.Join(root, "a.x")})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.(Noter).Notes(); !reflect.DeepEqual(n, []string{"ignored 1 non-JSON line(s) of extractor output"}) {
		t.Errorf("notes = %q", n)
	}
}

func TestExecPlaceholdersListFileEnvAndDir(t *testing.T) {
	root := t.TempDir()
	files := []string{filepath.Join(root, "dump"), filepath.Join(root, "two words.x")}
	ec := helperEC("dump", "--root={root}")
	ec.Env["EXTRACT_TEST_EXTRA"] = "extra-value"
	env := Env{Root: root}
	ex, _ := New("lang", "fake", ec)
	s, err := ex.Start(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var recs []Record
	// the helper reports under file "dump"; ask for that name so the records are kept
	err = s.Edges(context.Background(), []string{"dump"}, nil, func(_ string, rs []Record) error { recs = rs; return nil })
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%v err=%v", recs, err)
	}
	if want := "dump\n"; recs[0].Name != want {
		t.Errorf("list file contents = %q, want %q", recs[0].Name, want)
	}
	if !strings.HasSuffix(recs[0].In, "--root="+root) || !strings.Contains(recs[0].In, "--list|") {
		t.Errorf("args = %q", recs[0].In)
	}
	if recs[1].Name != "extra-value" {
		t.Errorf("entry env not passed: %q", recs[1].Name)
	}
	if a, _ := filepath.EvalSymlinks(recs[1].In); a != mustEval(t, root) {
		t.Errorf("working directory = %q, want %q", recs[1].In, root)
	}
	// several files: one per line, each newline-terminated
	ex2, _ := New("lang", "fake", helperEC("dump"))
	s2, _ := ex2.Start(context.Background(), env)
	defer s2.Close()
	var dump []Record
	s2.Edges(context.Background(), []string{"dump", files[1]}, nil, func(f string, rs []Record) error {
		if f == "dump" {
			dump = rs
		}
		return nil
	})
	if len(dump) == 0 || dump[0].Name != "dump\n"+files[1]+"\n" {
		t.Errorf("list file = %v", dump)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExecNonZeroExitKeepsStderrTail(t *testing.T) {
	root := t.TempDir()
	_, _, err := runEdges(t, helperEC("fail"), Env{Root: root}, []string{filepath.Join(root, "a.x")})
	if err == nil || !strings.Contains(err.Error(), "exit status 7") || !strings.HasSuffix(err.Error(), "TAIL") {
		t.Fatalf("err = %v", err)
	}
	if i := strings.LastIndex(err.Error(), ": "); len(err.Error())-i > stderrTail+3 {
		t.Errorf("stderr part longer than %d: %q", stderrTail, err.Error()[i:])
	}
}

func TestExecTimeoutAndCancel(t *testing.T) {
	root := t.TempDir()
	ec := helperEC("sleep")
	ec.TimeoutS = 1
	start := time.Now()
	_, _, err := runEdges(t, ec, Env{Root: root}, []string{filepath.Join(root, "a.x")})
	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("timeout took %v", time.Since(start))
	}
	// a cancelled caller context is returned as is
	ex, _ := New("lang", "fake", helperEC("sleep"))
	s, _ := ex.Start(context.Background(), Env{Root: root})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = s.Edges(ctx, []string{filepath.Join(root, "a.x")}, nil, func(string, []Record) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancel: %v", err)
	}
}

func TestExecCommandResolution(t *testing.T) {
	look := func(have ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, h := range have {
				if n == h {
					return "/x/" + n, nil
				}
			}
			return "", errors.New("no")
		}
	}
	ec := config.Extractor{Argv: []string{"main", "-x"}, CommandFallbacks: []string{"alt1", "alt2"}, CommandEnv: "MY_CMD"}
	env := func(v string) func(string) string { return func(string) string { return v } }
	for _, c := range []struct {
		name, over string
		have       []string
		want       string
	}{
		{"argv0 first", "", []string{"main", "alt1"}, "/x/main"},
		{"fallback order", "", []string{"alt2", "alt1"}, "/x/alt1"},
		{"second fallback", "", []string{"alt2"}, "/x/alt2"},
		{"nothing", "", nil, ""},
		{"override wins", "alt2", []string{"main", "alt2"}, "/x/alt2"},
		{"override missing: no fallback", "gone", []string{"main"}, ""},
	} {
		p, err := ResolveCommand(ec, env(c.over), look(c.have...))
		if c.want == "" {
			if !errors.Is(err, ErrExtractorUnavailable) {
				t.Errorf("%s: %q %v", c.name, p, err)
			}
			continue
		}
		if err != nil || p != c.want {
			t.Errorf("%s: %q %v", c.name, p, err)
		}
	}
	_, err := ResolveCommand(ec, env(""), look())
	if err == nil || !strings.Contains(err.Error(), `"main", "alt1", "alt2"`) {
		t.Errorf("message must name what was tried: %v", err)
	}
	// Start reports an unavailable command; the Exec seam skips resolution
	missing := config.Extractor{Type: config.ExtractorExec, Argv: []string{"definitely-not-installed-xyz"}}
	ex, _ := New("l", "n", missing)
	if _, err := ex.Start(context.Background(), Env{}); !errors.Is(err, ErrExtractorUnavailable) {
		t.Errorf("Start: %v", err)
	}
	seam := func(_ context.Context, name string, files []string) ([]byte, error) {
		return []byte(`{"t":"func","file":"` + files[0] + `","name":"` + name + `"}` + "\n"), nil
	}
	g, _, err := runEdges(t, missing, Env{Exec: seam}, []string{"/w/a.x"})
	if err != nil || len(g["/w/a.x"]) != 1 || g["/w/a.x"][0].Name != "n" && g["/w/a.x"][0].Name != "fake" {
		t.Errorf("seam: %v %v", g, err)
	}
}

func TestExecProvidesAndVersion(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		sym  *bool
		want bool
	}{{nil, false}, {&no, false}, {&yes, true}} {
		ex, _ := New("l", "n", config.Extractor{Type: config.ExtractorExec, Argv: []string{"c"}, Symbols: c.sym})
		s, err := ex.Start(context.Background(), Env{Exec: func(context.Context, string, []string) ([]byte, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		if p := s.Provides(); p.Symbols != c.want || !p.Edges {
			t.Errorf("symbols=%v: %+v", c.sym, p)
		}
	}
	ctx := context.Background()
	ex, _ := New("l", "n", config.Extractor{Type: config.ExtractorExec, Argv: []string{"c"}, Version: "9"})
	if v := ex.Version(ctx); v != "9" {
		t.Errorf("version = %q", v)
	}
	ps := config.BuiltinExtractors()[config.BuiltinPowerShellAST]
	ex, _ = New("l", "ps", ps)
	_, _, hash, _ := embeddedFile("powershell-ast.ps1")
	if v := ex.Version(ctx); v != hash || v == "" {
		t.Errorf("embedded version = %q want %q", v, hash)
	}
	ex, _ = New("l", "n", config.Extractor{Type: config.ExtractorExec, Argv: []string{"c"}})
	if v := ex.Version(ctx); v != "" {
		t.Errorf("no version = %q", v)
	}
	if _, err := New("l", "n", config.Extractor{Type: config.ExtractorExec}); err == nil {
		t.Error("empty argv must be an error")
	}
}

func TestGroupByFileTolerantMatching(t *testing.T) {
	files := []string{"/w/Dir/A.ps1", "/w/b.ps1"}
	recs := []Record{{T: "x", File: "/w/Dir/A.ps1"}, {T: "x", File: "/w/Dir/../b.ps1"}, {T: "x", File: "/W/DIR/a.PS1"}, {T: "x", File: "/w/other.ps1"}}
	by, dropped := groupByFile(files, recs)
	if len(by["/w/Dir/A.ps1"]) != 2 || len(by["/w/b.ps1"]) != 1 || dropped != 1 {
		t.Errorf("by=%v dropped=%d", by, dropped)
	}
}

// config.KnownPlaceholder mirrors IsKnownPlaceholder (config cannot import this package).
func TestConfigPlaceholderCheckMatchesExtract(t *testing.T) {
	for _, tok := range []string{"{root}", "{listFile}", "{embedded:powershell-ast.ps1}", "{embedded:nope.ps1}", "{embedded:}", "{x}", "{}", "{ROOT}"} {
		if IsKnownPlaceholder(tok) != config.KnownPlaceholder(tok) {
			t.Errorf("%s: extract=%v config=%v", tok, IsKnownPlaceholder(tok), config.KnownPlaceholder(tok))
		}
	}
}

func TestResolveCommandInExpandsRoot(t *testing.T) {
	root := t.TempDir()
	want := root + "/tools/extract"
	look := func(c string) (string, error) {
		if c == want {
			return c, nil
		}
		return "", errors.New("not found")
	}
	ec := config.Extractor{Type: config.ExtractorExec, Argv: []string{"{root}/tools/extract", "{listFile}"}}
	if got, err := ResolveCommandIn(ec, root, func(string) string { return "" }, look); err != nil || got != want {
		t.Errorf("= %q, %v", got, err)
	}
	ec.Argv[0] = "{root}/missing"
	if _, err := ResolveCommandIn(ec, root, func(string) string { return "" }, look); !errors.Is(err, ErrExtractorUnavailable) {
		t.Errorf("missing: %v", err)
	}
}

// Output made only of non-JSON lines is a failed run, not an empty result:
// otherwise every requested file would be stored with no symbols and never retried.
func TestExecGarbageOnlyOutputIsAnError(t *testing.T) {
	root := t.TempDir()
	files := []string{filepath.Join(root, "a.x")}
	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("garbage-only output must be an error")
		}
		for _, want := range []string{`"fake"`, "usage: extractor [-h] --list FILE", "no JSON"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "second line") {
			t.Errorf("only the first skipped line is quoted: %q", err)
		}
	}
	t.Run("subprocess", func(t *testing.T) {
		g, _, err := runEdges(t, helperEC("garbage"), Env{Root: root}, files)
		check(t, err)
		if len(g) != 0 {
			t.Errorf("nothing may be emitted: %v", g)
		}
	})
	t.Run("seam", func(t *testing.T) {
		seam := func(context.Context, string, []string) ([]byte, error) {
			return []byte("usage: extractor [-h] --list FILE\nsecond line\n"), nil
		}
		_, _, err := runEdges(t, helperEC("garbage"), Env{Root: root, Exec: seam}, files)
		check(t, err)
	})
	t.Run("long line is truncated", func(t *testing.T) {
		seam := func(context.Context, string, []string) ([]byte, error) {
			return []byte(strings.Repeat("z", 5000) + "\n"), nil
		}
		_, _, err := runEdges(t, helperEC("garbage"), Env{Root: root, Exec: seam}, files)
		if err == nil || len(err.Error()) > 600 {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("no output at all stays valid", func(t *testing.T) {
		g, _, err := runEdges(t, helperEC("silent"), Env{Root: root}, files)
		if err != nil || len(g) != 1 || len(g[files[0]]) != 0 {
			t.Errorf("g=%v err=%v", g, err)
		}
	})
	t.Run("blank lines only stay valid", func(t *testing.T) {
		seam := func(context.Context, string, []string) ([]byte, error) { return []byte("\n  \n"), nil }
		if _, _, err := runEdges(t, helperEC("silent"), Env{Root: root, Exec: seam}, files); err != nil {
			t.Error(err)
		}
	})
	t.Run("mixed output keeps the note", func(t *testing.T) {
		g, s, err := runEdges(t, helperEC("noise"), Env{Root: root}, files)
		if err != nil || len(g[files[0]]) != 1 || len(s.(Noter).Notes()) != 1 {
			t.Errorf("g=%v err=%v", g, err)
		}
	})
}
