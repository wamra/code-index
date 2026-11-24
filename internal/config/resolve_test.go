package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolvedNormalisesLegacyAndIsIdempotent(t *testing.T) {
	yes := true
	cases := map[string]Config{
		"legacy":             {Languages: map[string]Language{"sh": {Command: "bsh", Args: []string{"start"}, Env: map[string]string{"A": "b"}, Extensions: []string{".sh"}}}},
		"legacy + extractor": {Languages: map[string]Language{"ps": {Command: "pwsh", Extensions: []string{".ps1"}, Extractor: "powershell-ast"}}},
		"new style": {
			Languages:  map[string]Language{"ps": {Extensions: []string{".ps1"}, Extractor: NewExtractorNames("pses", "powershell-ast")}},
			Extractors: map[string]Extractor{"pses": {Type: ExtractorLSP, Argv: []string{"pwsh", "-File", "s.ps1"}, LanguageID: "powershell", StartupTimeoutS: 7}},
		},
		"exec only": {
			Languages:  map[string]Language{"b": {Extensions: []string{".sh"}, Extractor: "wb"}},
			Extractors: map[string]Extractor{"wb": {Type: ExtractorExec, Argv: []string{"wb"}, Symbols: &yes}},
		},
	}
	for name, c := range cases {
		once := c.Resolved()
		twice := once.Resolved()
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("%s: Resolved is not idempotent:\n%+v\n%+v", name, once, twice)
		}
	}
	// the caller's maps are not written through
	c := cases["legacy"]
	c.Resolved()
	if len(c.Extractors) != 0 || c.Languages["sh"].Extractor != "" {
		t.Errorf("Resolved changed its receiver: %+v", c)
	}

	r := cases["legacy"].Resolved()
	imp := r.Extractors["sh-lsp"]
	if imp.Type != ExtractorLSP || !reflect.DeepEqual(imp.Argv, []string{"bsh", "start"}) || imp.Env["A"] != "b" ||
		imp.LanguageID != "sh" || imp.StartupTimeoutS != 120 || imp.RequestTimeoutS != 30 {
		t.Errorf("implicit extractor = %+v", imp)
	}
	if got := r.Languages["sh"].Extractor.Names(); !reflect.DeepEqual(got, []string{"sh-lsp"}) {
		t.Errorf("names = %q", got)
	}
	r = cases["legacy + extractor"].Resolved()
	if got := r.Languages["ps"].Extractor.Names(); !reflect.DeepEqual(got, []string{"ps-lsp", "powershell-ast"}) {
		t.Errorf("names = %q", got)
	}
	if _, copied := r.Extractors["powershell-ast"]; copied {
		t.Error("built-ins are referenced, not copied into Extractors")
	}
	// a new-style language gets the legacy fields of its first lsp extractor
	l := cases["new style"].Resolved().Languages["ps"]
	if l.Command != "pwsh" || !reflect.DeepEqual(l.Args, []string{"-File", "s.ps1"}) || l.LanguageID != "powershell" || l.StartupTimeoutS != 7 || l.RequestTimeoutS != 30 {
		t.Errorf("legacy fields = %+v", l)
	}
	if e := cases["exec only"].Resolved().Extractors["wb"]; e.TimeoutS != 600 || e.StartupTimeoutS != 0 {
		t.Errorf("exec defaults: %+v", e)
	}
}

func TestExtractorsFor(t *testing.T) {
	// a legacy language as astCfg builds it: extractor set after a first Resolved
	c := Config{Languages: map[string]Language{"fake": {Command: "c", Extensions: []string{".ps1", ".psm1", ".psd1"}}}}.Resolved()
	l := c.Languages["fake"]
	l.Extractor = "powershell-ast"
	c.Languages["fake"] = l
	got := c.ExtractorsFor("fake")
	if len(got) != 2 || got[0].Name != "fake-lsp" || got[0].Type != ExtractorLSP || got[1].Name != "powershell-ast" {
		t.Fatalf("got %+v", got)
	}
	ast := got[1]
	if ast.TimeoutS != 600 || !reflect.DeepEqual(ast.Extensions, []string{".ps1", ".psm1"}) || ast.Symbols == nil || *ast.Symbols {
		t.Errorf("powershell-ast next to an lsp extractor: %+v", ast)
	}
	if got[0].LanguageID != "fake" {
		t.Errorf("lsp language_id = %q", got[0].LanguageID)
	}
	if c.ExtractorsFor("nope") != nil {
		t.Error("unknown language")
	}
	// alone, an exec extractor provides symbols; extensions default to the language's
	solo := Config{
		Languages:  map[string]Language{"b": {Extensions: []string{".sh", ".bash"}, Extractor: "wb"}},
		Extractors: map[string]Extractor{"wb": {Type: ExtractorExec, Argv: []string{"wb", "{listFile}"}}},
	}.ExtractorsFor("b")
	if len(solo) != 1 || solo[0].Symbols == nil || !*solo[0].Symbols || !reflect.DeepEqual(solo[0].Extensions, []string{".sh", ".bash"}) {
		t.Errorf("solo = %+v", solo)
	}
	// a config entry replaces the built-in as a whole
	over := Config{
		Languages:  map[string]Language{"p": {Extensions: []string{".ps1"}, Extractor: "powershell-ast"}},
		Extractors: map[string]Extractor{"powershell-ast": {Type: ExtractorExec, Argv: []string{"mine"}}},
	}.ExtractorsFor("p")
	if len(over) != 1 || !reflect.DeepEqual(over[0].Argv, []string{"mine"}) || over[0].CommandEnv != "" {
		t.Errorf("override = %+v", over)
	}
}

func TestExtractorValidation(t *testing.T) {
	ps := func(extra map[string]Extractor, exts []string, names ...string) Config {
		return Config{
			Languages:  map[string]Language{"l": {Extensions: exts, Extractor: NewExtractorNames(names...)}},
			Extractors: extra,
		}
	}
	exe := func(mod func(*Extractor)) map[string]Extractor {
		e := Extractor{Type: ExtractorExec, Argv: []string{"tool", "{listFile}"}}
		mod(&e)
		return map[string]Extractor{"x": e}
	}
	lsp := func(mod func(*Extractor)) map[string]Extractor {
		e := Extractor{Type: ExtractorLSP, Argv: []string{"srv"}}
		mod(&e)
		return map[string]Extractor{"x": e}
	}
	yes := true
	for _, c := range []struct {
		name string
		cfg  Config
		want string // substring of the error; empty: valid
	}{
		{"valid exec", ps(exe(func(*Extractor) {}), []string{".a"}, "x"), ""},
		{"valid lsp", ps(lsp(func(e *Extractor) { e.LanguageID = "l" }), []string{".a"}, "x"), ""},
		{"no extractor", ps(nil, []string{".a"}), "extractor is required"},
		{"no extensions", ps(exe(func(*Extractor) {}), nil, "x"), "extensions is required"},
		{"unknown extractor", ps(nil, []string{".a"}, "nope"), "unknown extractor"},
		{"bad type", ps(exe(func(e *Extractor) { e.Type = "grpc" }), []string{".a"}, "x"), "type"},
		{"empty argv", ps(exe(func(e *Extractor) { e.Argv = nil }), []string{".a"}, "x"), "argv is required"},
		{"unknown placeholder", ps(exe(func(e *Extractor) { e.Argv = []string{"t", "{bogus}"} }), []string{".a"}, "x"), "unknown placeholder {bogus}"},
		{"unknown embedded", ps(exe(func(e *Extractor) { e.Argv = []string{"t", "{embedded:nope.ps1}"} }), []string{".a"}, "x"), "unknown placeholder"},
		{"listFile in lsp", ps(lsp(func(e *Extractor) { e.Argv = []string{"srv", "{listFile}"} }), []string{".a"}, "x"), "{listFile} is only"},
		{"placeholder in version_argv", ps(exe(func(e *Extractor) { e.VersionArgv = []string{"t", "{root}"} }), []string{".a"}, "x"), "version_argv"},
		{"extension without dot", ps(exe(func(e *Extractor) { e.Extensions = []string{"a"} }), []string{".a"}, "x"), "must start with a dot"},
		{"extension not in language", ps(exe(func(e *Extractor) { e.Extensions = []string{".b"} }), []string{".a"}, "x"), "not an extension of language l"},
		{"extension subset, case-insensitive", ps(exe(func(e *Extractor) { e.Extensions = []string{".A"} }), []string{".a", ".c"}, "x"), ""},
		{"exec field on lsp: timeout_s", ps(lsp(func(e *Extractor) { e.TimeoutS = 5 }), []string{".a"}, "x"), "timeout_s only applies to exec"},
		{"exec field on lsp: symbols", ps(lsp(func(e *Extractor) { e.Symbols = &yes }), []string{".a"}, "x"), "symbols only applies to exec"},
		{"lsp field on exec", ps(exe(func(e *Extractor) { e.RequestTimeoutS = 5 }), []string{".a"}, "x"), "request_timeout_s only applies to lsp"},
		{"bad name", Config{Languages: map[string]Language{"l": {Extensions: []string{".a"}, Extractor: "a b"}}, Extractors: map[string]Extractor{"a b": {Type: ExtractorExec, Argv: []string{"t"}}}}, "must match"},
		{"built-in on a language without its extensions", ps(nil, []string{".x"}, "powershell-ast"), ""},
	} {
		err := c.cfg.Resolved().Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestLayeredExtractors(t *testing.T) {
	dir := t.TempDir()
	g, r := filepath.Join(dir, "g.json"), filepath.Join(dir, "r.json")
	os.WriteFile(g, []byte(`{"languages":{"a":{"extensions":[".a"],"extractor":["ga","powershell-ast"]}},
		"extractors":{"ga":{"type":"exec","argv":["g"]},"gb":{"type":"exec","argv":["gb"]}}}`), 0o644)
	os.WriteFile(r, []byte(`{"extractors":{"ga":{"type":"exec","argv":["repo"],"timeout_s":5},"rc":{"type":"lsp","argv":["rc"]}}}`), 0o644)
	c, src, err := LoadLayered(g, r, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Extractors["ga"].Argv, []string{"repo"}) || c.Extractors["ga"].TimeoutS != 5 || c.Extractors["gb"].Argv[0] != "gb" || c.Extractors["rc"].Type != ExtractorLSP {
		t.Errorf("merged extractors: %+v", c.Extractors)
	}
	for k, want := range map[string]string{"extractors.ga": r, "extractors.gb": g, "extractors.rc": r, "languages.a": g} {
		if src[k] != want {
			t.Errorf("source[%s] = %q, want %q", k, src[k], want)
		}
	}
}

func TestEffectiveShowsNewShape(t *testing.T) {
	c := Config{Languages: map[string]Language{"ps": {Command: "pwsh", Extensions: []string{".ps1", ".psm1"}, Extractor: "powershell-ast"}}}
	var got struct {
		Sources    map[string]string          `json:"_sources"`
		Languages  map[string]json.RawMessage `json:"languages"`
		Extractors map[string]Extractor       `json:"extractors"`
	}
	if err := json.Unmarshal(Effective(c, Sources{"languages.ps": "/cfg.json"}, env(nil)), &got); err != nil {
		t.Fatal(err)
	}
	if lang := string(got.Languages["ps"]); strings.Contains(lang, "command") || !strings.Contains(lang, `"extensions"`) || !strings.Contains(lang, `"ps-lsp"`) || !strings.Contains(lang, `"powershell-ast"`) {
		t.Errorf("language = %s", lang)
	}
	if got.Extractors["ps-lsp"].Type != ExtractorLSP || got.Extractors["powershell-ast"].Type != ExtractorExec || got.Extractors["powershell-ast"].TimeoutS != 600 {
		t.Errorf("extractors = %+v", got.Extractors)
	}
	if got.Sources["extractors.ps-lsp"] != "/cfg.json" || got.Sources["extractors.powershell-ast"] != "built-in" {
		t.Errorf("sources = %v", got.Sources)
	}
}
