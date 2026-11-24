package extract

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/config"
)

func TestMain(m *testing.M) {
	// the test binary doubles as the version command
	if os.Getenv("EXTRACT_TEST_VERSION") != "" {
		os.Stdout.WriteString("  " + os.Getenv("EXTRACT_TEST_VERSION") + "  \nsecond line\n")
		return
	}
	os.Exit(m.Run())
}

// The recorded output of wammybash extract parses without a skipped line, and
// every call in it carries a resolution that normalises to a canonical value.
func TestParseLinesWammybashGolden(t *testing.T) {
	f, err := os.Open("testdata/wammybash-extract.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, skipped, err := ParseLines(f)
	if err != nil || skipped != 0 || len(recs) != 35 {
		t.Fatalf("recs=%d skipped=%d err=%v", len(recs), skipped, err)
	}
	counts := map[string]int{}
	res := map[string]int{}
	for _, r := range recs {
		counts[r.T]++
		if r.T == TypeCall {
			got, ok := NormalizeResolution(r.T, r.Resolution, r.Reason)
			if !ok || r.To == "" {
				t.Errorf("call %+v: resolution %q ok=%v", r, got, ok)
			}
			res[got+" "+r.Reason]++
		}
	}
	if counts["func"] != 13 || counts["alias"] != 1 || counts["dotsource"] != 4 || counts["call"] != 15 || counts["parse_error"] != 2 {
		t.Errorf("counts = %v", counts)
	}
	for _, want := range []string{"exact direct", "alias alias", "candidate variable-constant", "candidate string-eval", "candidate string-trap"} {
		if res[want] == 0 {
			t.Errorf("no call normalised to %q: %v", want, res)
		}
	}
}

func TestParseLinesSkipsNoiseAndKeepsV2Keys(t *testing.T) {
	in := "WARNING: something\n\n" +
		`{"t":"call","name":"X","file":"/a.sh","line":1,"col":1,"to":"/b.sh","toLine":3,"toCol":4,"resolution":"exact","reason":"direct"}` + "\n" +
		"{broken\n" + `{"nope":1}` + "\n" + `{"t":"call","line":"3"}` + "\n" +
		`{"t":"symbol","name":"s","kind":"method","parent":0,"selLine":2,"selCol":5,"qualified":"A.s","unknown":true}` + "\n" +
		`{"t":"func","name":"g"}` + "\n"
	recs, skipped, err := ParseLines(strings.NewReader(in))
	if err != nil || len(recs) != 3 || skipped != 4 {
		t.Fatalf("recs=%+v skipped=%d err=%v", recs, skipped, err)
	}
	c := recs[0]
	if c.To != "/b.sh" || c.ToLine != 3 || c.ToCol != 4 || c.Resolution != "exact" || c.Reason != "direct" {
		t.Errorf("call = %+v", c)
	}
	s := recs[1]
	if s.Parent == nil || *s.Parent != 0 || s.Kind != "method" || s.SelLine != 2 || s.SelCol != 5 || s.Qualified != "A.s" || !s.IsSymbol() {
		t.Errorf("symbol = %+v", s)
	}
	if recs[2].Parent != nil || !recs[2].IsSymbol() {
		t.Errorf("func = %+v", recs[2])
	}
	// omitempty: a zero parent ordinal survives a round trip, an absent one stays absent
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"parent":0`) {
		t.Errorf("marshal lost parent 0: %s", b)
	}
	b, _ = json.Marshal(recs[2])
	if strings.Contains(string(b), "parent") {
		t.Errorf("marshal wrote an absent parent: %s", b)
	}
}

func TestNormalizeResolution(t *testing.T) {
	type c struct {
		t, res, reason, want string
		ok                   bool
	}
	for _, tc := range []c{
		// call: required
		{TypeCall, "exact", "", "exact", true},
		{TypeCall, "alias", "", "alias", true},
		{TypeCall, "candidate", "name-match", "candidate", true},
		{TypeCall, "resolved", "direct", "exact", true},
		{TypeCall, "resolved", "", "exact", true},
		{TypeCall, "resolved", "alias", "alias", true},
		{TypeCall, "exact", "alias", "exact", true}, // the reason only changes the synonym
		{TypeCall, "", "", "", false},
		{TypeCall, "unresolved", "", "", false},
		{TypeCall, "dynamic", "", "", false},
		{TypeCall, "maybe", "", "", false},
		// invoke, dotsource, import: optional
		{TypeInvoke, "", "", "exact", true},
		{TypeInvoke, "resolved", "direct", "exact", true},
		{TypeInvoke, "resolved", "alias", "alias", true},
		{TypeInvoke, "candidate", "", "candidate", true},
		{TypeInvoke, "unresolved", "not-found", "unresolved", true},
		{TypeDotSource, "", "", "exact", true},
		{TypeDotSource, "exact", "", "exact", true},
		{TypeDotSource, "unresolved", "skipped-for-size", "unresolved", true},
		{TypeDotSource, "maybe", "", "unresolved", true},
		{TypeImport, "resolved", "", "exact", true},
		// dynamic: always dynamic
		{TypeDynamic, "", "", "dynamic", true},
		{TypeDynamic, "dynamic", "string-eval", "dynamic", true},
		{TypeDynamic, "whatever", "", "dynamic", true},
		// other types have none
		{TypeSymbol, "exact", "", "", true},
		{TypeAlias, "", "", "", true},
		{TypeError, "unresolved", "", "", true},
	} {
		got, ok := NormalizeResolution(tc.t, tc.res, tc.reason)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeResolution(%q, %q, %q) = %q, %v; want %q, %v", tc.t, tc.res, tc.reason, got, ok, tc.want, tc.ok)
		}
	}
	if !(Strength(ResExact) > Strength(ResAlias) && Strength(ResAlias) > Strength(ResCandidate) && Strength(ResCandidate) > Strength(ResDynamic) && Strength(ResDynamic) == Strength(ResUnresolved)) {
		t.Error("strength order is exact > alias > candidate > the rest")
	}
}

func TestFingerprint(t *testing.T) {
	base := []Part{
		{Name: "pses", Type: "lsp", Argv: []string{"pwsh", "-File", "/x"}, Version: "4.1"},
		{Name: "ast", Type: "exec", Argv: []string{"pwsh", "{listFile}"}, Version: ""},
	}
	fp := Fingerprint(base)
	if len(fp) != 64 || fp != Fingerprint(base) {
		t.Fatalf("fingerprint %q is not stable sha256 hex", fp)
	}
	mut := func(f func(p []Part)) string {
		c := make([]Part, len(base))
		for i, p := range base {
			p.Argv = append([]string(nil), p.Argv...)
			c[i] = p
		}
		f(c)
		return Fingerprint(c)
	}
	seen := map[string]string{fp: "base"}
	for name, got := range map[string]string{
		"version":   mut(func(p []Part) { p[0].Version = "4.2" }),
		"name":      mut(func(p []Part) { p[1].Name = "ast2" }),
		"type":      mut(func(p []Part) { p[1].Type = "lsp" }),
		"argv":      mut(func(p []Part) { p[0].Argv[2] = "/y" }),
		"argv-len":  mut(func(p []Part) { p[0].Argv = p[0].Argv[:2] }),
		"order":     mut(func(p []Part) { p[0], p[1] = p[1], p[0] }),
		"one only":  Fingerprint(base[:1]),
		"no parts":  Fingerprint(nil),
		"empty ver": mut(func(p []Part) { p[0].Version = "" }),
	} {
		if other, dup := seen[got]; dup {
			t.Errorf("%s gives the same fingerprint as %s", name, other)
		}
		seen[got] = name
	}
	// nil and empty argv are the same identity
	a := Fingerprint([]Part{{Name: "n", Type: "exec"}})
	b := Fingerprint([]Part{{Name: "n", Type: "exec", Argv: []string{}}})
	if a != b {
		t.Error("nil argv and empty argv differ")
	}
}

// The contract fixes the schema version at 2; changing it changes every fingerprint.
func TestSchemaVersionIsTwo(t *testing.T) {
	if SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, the contract says 2", SchemaVersion)
	}
}

func TestPlaceholdersAndExpandArgv(t *testing.T) {
	if got := Placeholders("--root={root}/x {listFile} {embedded:a.ps1} { } {nope} lone { brace"); strings.Join(got, "|") != "{root}|{listFile}|{embedded:a.ps1}|{ }|{nope}" {
		t.Errorf("placeholders = %q", got)
	}
	for tok, want := range map[string]bool{"{root}": true, "{listFile}": true, "{embedded:powershell-ast.ps1}": true, "{embedded:nope.ps1}": false, "{embedded:}": false, "{x}": false} {
		if IsKnownPlaceholder(tok) != want {
			t.Errorf("IsKnownPlaceholder(%s) = %v", tok, !want)
		}
	}
	v := Vars{Root: "/w", ListFile: "/tmp/l.txt", Embedded: func(n string) (string, error) { return "/cache/" + n, nil }}
	got, err := ExpandArgv([]string{"tool", "--root", "{root}", "--root={root}/sub", "-File", "{embedded:s.ps1}", "{listFile}"}, v)
	if err != nil || strings.Join(got, " ") != "tool --root /w --root=/w/sub -File /cache/s.ps1 /tmp/l.txt" {
		t.Errorf("expand = %q err=%v", got, err)
	}
	if _, err := ExpandArgv([]string{"x", "{bogus}"}, v); err == nil || !strings.Contains(err.Error(), "{bogus}") {
		t.Errorf("unknown placeholder: %v", err)
	}
	if _, err := ExpandArgv([]string{"{listFile}"}, Vars{Root: "/w"}); err == nil {
		t.Error("{listFile} without a list file must be an error")
	}
}

// IdentityArgv is stable across calls, keeps {listFile} literal and names the
// embedded file by content hash without writing it.
func TestIdentityArgv(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("LocalAppData", cache)
	argv := config.BuiltinExtractors()[config.BuiltinPowerShellAST].Argv
	a, err := IdentityArgv(argv, "/w")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := IdentityArgv(argv, "/w")
	if strings.Join(a, " ") != strings.Join(b, " ") {
		t.Error("identity argv changes between calls")
	}
	joined := strings.Join(a, " ")
	if !strings.Contains(joined, "-ListFile {listFile}") || !strings.Contains(joined, "embedded:powershell-ast.ps1@") || strings.Contains(joined, cache) {
		t.Errorf("identity argv = %q", joined)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("IdentityArgv wrote into the cache dir: %v", entries)
	}
	// EmbeddedPath writes the content-hash name once and reuses it
	p, err := EmbeddedPath("powershell-ast.ps1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(p), "powershell-ast-") || filepath.Ext(p) != ".ps1" {
		t.Errorf("embedded path = %s", p)
	}
	if p2, err := EmbeddedPath("powershell-ast.ps1"); err != nil || p2 != p {
		t.Errorf("second call = %s %v", p2, err)
	}
	if _, err := EmbeddedPath("missing.ps1"); err == nil {
		t.Error("an unknown embedded file must be an error")
	}
}

func TestConfiguredVersion(t *testing.T) {
	ctx := context.Background()
	if v := ConfiguredVersion(ctx, config.Extractor{Version: "1.2", VersionArgv: []string{"nope-not-a-command"}}); v != "1.2" {
		t.Errorf("static version = %q", v)
	}
	if v := ConfiguredVersion(ctx, config.Extractor{}); v != "" {
		t.Errorf("no version = %q", v)
	}
	if v := ConfiguredVersion(ctx, config.Extractor{VersionArgv: []string{"definitely-not-installed-xyz", "version"}}); v != "" {
		t.Errorf("missing command = %q", v)
	}
	ec := config.Extractor{VersionArgv: []string{os.Args[0], "-test.run=^$"}, Env: map[string]string{"EXTRACT_TEST_VERSION": "wammybash 1.4"}}
	if v := ConfiguredVersion(ctx, ec); v != "wammybash 1.4" {
		t.Errorf("version_argv = %q", v)
	}
	if runtime.GOOS != "windows" {
		if v := ConfiguredVersion(ctx, config.Extractor{VersionArgv: []string{"false"}}); v != "" {
			t.Errorf("failing command = %q", v)
		}
	}
}

func TestFactoryStubsAndUnknownType(t *testing.T) {
	for _, typ := range []string{config.ExtractorExec} {
		_, err := New("lang", "x", config.Extractor{Type: typ, Argv: []string{"c"}})
		if err == nil || !strings.Contains(err.Error(), "not implemented in this build") {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, err := New("lang", "x", config.Extractor{Type: "grpc"}); err == nil || !strings.Contains(err.Error(), `unknown type "grpc"`) {
		t.Errorf("unknown type: %v", err)
	}
	if !errors.Is(ErrExtractorUnavailable, ErrExtractorUnavailable) {
		t.Error("sentinel")
	}
}
