package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExtractorNamesJSON(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
		out  string // marshalled form
	}{
		{`"powershell-ast"`, []string{"powershell-ast"}, `"powershell-ast"`},
		{`["pses","powershell-ast"]`, []string{"pses", "powershell-ast"}, `["pses","powershell-ast"]`},
		{`["only"]`, []string{"only"}, `"only"`}, // one name is written back as a string
		{`null`, nil, `null`},
		{`""`, nil, `null`},
	} {
		var n ExtractorNames
		if err := json.Unmarshal([]byte(c.in), &n); err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if !reflect.DeepEqual(n.Names(), c.want) {
			t.Errorf("%s: names = %q, want %q", c.in, n.Names(), c.want)
		}
		b, err := json.Marshal(n)
		if err != nil || string(b) != c.out {
			t.Errorf("%s: marshal = %s (%v), want %s", c.in, b, err, c.out)
		}
	}
	for _, bad := range []string{`[]`, `["a",""]`, `["a,b"]`, `"a,b"`, `5`, `{"a":1}`, `[1]`} {
		var n ExtractorNames
		if err := json.Unmarshal([]byte(bad), &n); err == nil {
			t.Errorf("%s: want an error, got %q", bad, n.Names())
		}
	}
	// a string constant is a valid Go value, and omitempty drops the empty one
	l := Language{Extractor: "powershell-ast", Extensions: []string{".ps1"}}
	if b, _ := json.Marshal(l); !strings.Contains(string(b), `"extractor":"powershell-ast"`) {
		t.Errorf("language = %s", b)
	}
	if b, _ := json.Marshal(Language{Extensions: []string{".x"}}); strings.Contains(string(b), "extractor") || strings.Contains(string(b), "command") {
		t.Errorf("empty extractor/command must be omitted: %s", b)
	}
	// map values are not addressable: the value receiver must work inside a map
	b, err := json.Marshal(map[string]ExtractorNames{"x": NewExtractorNames("a", "b")})
	if err != nil || string(b) != `{"x":["a","b"]}` {
		t.Errorf("map marshal = %s %v", b, err)
	}
}

// The example config of extractors/README.md decodes with DisallowUnknownFields.
func TestExtractorsConfigExampleDecodes(t *testing.T) {
	const doc = `{
  "languages": {
    "powershell": { "extensions": [".ps1", ".psm1", ".psd1"], "extractor": ["pses", "powershell-ast"] },
    "bash":       { "extensions": [".sh", ".bash"], "extractor": "wammybash" }
  },
  "extractors": {
    "pses":           { "type": "lsp",  "argv": ["pwsh", "-NoLogo", "-NoProfile", "-File", "/path/start-pses.ps1"], "language_id": "powershell", "startup_timeout_s": 120 },
    "powershell-ast": { "type": "exec", "argv": ["pwsh", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", "{embedded:powershell-ast.ps1}", "-ListFile", "{listFile}"], "command_fallbacks": ["powershell"], "command_env": "CODE_INDEX_POWERSHELL", "extensions": [".ps1", ".psm1"] },
    "wammybash":      { "type": "exec", "argv": ["wammybash", "extract", "--root", "{root}", "--list-file", "{listFile}"], "version_argv": ["wammybash", "version"] }
  }
}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, found, err := Load(path, func(string) string { return "" })
	if err != nil || !found {
		t.Fatalf("load: %v found=%v", err, found)
	}
	if got := c.Languages["powershell"].Extractor.Names(); !reflect.DeepEqual(got, []string{"pses", "powershell-ast"}) {
		t.Errorf("powershell extractors = %q", got)
	}
	if got := c.Languages["bash"].Extractor.Names(); !reflect.DeepEqual(got, []string{"wammybash"}) {
		t.Errorf("bash extractors = %q", got)
	}
	p := c.Extractors["pses"]
	if p.Type != ExtractorLSP || p.LanguageID != "powershell" || p.StartupTimeoutS != 120 || len(p.Argv) != 5 {
		t.Errorf("pses = %+v", p)
	}
	if w := c.Extractors["wammybash"]; w.Type != ExtractorExec || !reflect.DeepEqual(w.VersionArgv, []string{"wammybash", "version"}) {
		t.Errorf("wammybash = %+v", w)
	}
	// the built-in entry is exactly the example's powershell-ast
	var ex struct {
		Extractors map[string]Extractor `json:"extractors"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(doc)))
	if err := dec.Decode(&ex); err != nil {
		t.Fatal(err)
	}
	if got := BuiltinExtractors()[BuiltinPowerShellAST]; !reflect.DeepEqual(got, ex.Extractors["powershell-ast"]) {
		t.Errorf("built-in powershell-ast = %+v\nexample = %+v", got, ex.Extractors["powershell-ast"])
	}
	// every call returns a fresh copy
	b := BuiltinExtractors()
	b[BuiltinPowerShellAST].Argv[0] = "changed"
	if BuiltinExtractors()[BuiltinPowerShellAST].Argv[0] != "pwsh" {
		t.Error("BuiltinExtractors shares state between calls")
	}
	// unknown fields in an extractor entry are still rejected
	if err := os.WriteFile(path, []byte(`{"extractors":{"x":{"type":"exec","argv":["a"],"bogus":1}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("unknown field: %v", err)
	}
}

// Validation holes: each of these used to pass.
func TestValidateExtractorHoles(t *testing.T) {
	ps := Language{Extensions: []string{".ps1"}}
	with := func(l Language, exs map[string]Extractor) Config {
		return Config{Languages: map[string]Language{"ps": l}, Extractors: exs}
	}
	lsp := func(mod func(*Extractor)) Extractor {
		e := Extractor{Type: ExtractorLSP, Argv: []string{"pwsh", "-File", "s.ps1"}}
		if mod != nil {
			mod(&e)
		}
		return e
	}
	exe := func(mod func(*Extractor)) Extractor {
		e := Extractor{Type: ExtractorExec, Argv: []string{"tool", "{listFile}"}}
		if mod != nil {
			mod(&e)
		}
		return e
	}
	lang := func(command string, names ...string) Language {
		l := ps
		l.Command = command
		l.Extractor = NewExtractorNames(names...)
		return l
	}
	for _, c := range []struct {
		name string
		cfg  Config
		want string // "" = valid
	}{
		{"command + lsp extractor", with(lang("srv", "pses"), map[string]Extractor{"pses": lsp(nil)}), "languages.ps: both command and an lsp extractor (pses); use one"},
		{"command + lsp among several", with(lang("srv", "tool", "pses"), map[string]Extractor{"pses": lsp(nil), "tool": exe(nil)}), "both command and an lsp extractor (pses)"},
		{"legacy command + builtin exec", with(lang("srv", "powershell-ast"), nil), ""},
		{"legacy command alone", with(lang("srv"), nil), ""},
		{"lsp extractor alone", with(lang("", "pses"), map[string]Extractor{"pses": lsp(nil)}), ""},
		{"negative timeout_s", with(lang("", "tool"), map[string]Extractor{"tool": exe(func(e *Extractor) { e.TimeoutS = -1 })}), "extractors.tool.timeout_s"},
		{"negative startup_timeout_s", with(lang("", "pses"), map[string]Extractor{"pses": lsp(func(e *Extractor) { e.StartupTimeoutS = -5 })}), "extractors.pses.startup_timeout_s"},
		{"negative request_timeout_s", with(lang("", "pses"), map[string]Extractor{"pses": lsp(func(e *Extractor) { e.RequestTimeoutS = -5 })}), "extractors.pses.request_timeout_s"},
		{"zero timeouts mean default", with(lang("", "tool"), map[string]Extractor{"tool": exe(func(e *Extractor) { e.TimeoutS = 0 })}), ""},
		{"duplicate name", with(lang("", "tool", "tool"), map[string]Extractor{"tool": exe(nil)}), `languages.ps.extractor: "tool" is listed twice`},
		{"empty version_argv[0]", with(lang("", "tool"), map[string]Extractor{"tool": exe(func(e *Extractor) { e.VersionArgv = []string{"", "v"} })}), "extractors.tool.version_argv"},
		{"empty argv[0]", with(lang("", "tool"), map[string]Extractor{"tool": exe(func(e *Extractor) { e.Argv = []string{"", "x"} })}), "extractors.tool.argv"},
		{"empty argv", with(lang("", "tool"), map[string]Extractor{"tool": exe(func(e *Extractor) { e.Argv = nil })}), "extractors.tool.argv"},
	} {
		err := c.cfg.Resolved().Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error = %v, want it to contain %q", c.name, err, c.want)
		}
	}
	// a legacy command stays legal after normalisation (Resolved twice)
	r := with(lang("srv"), nil).Resolved().Resolved()
	if err := r.Validate(); err != nil {
		t.Errorf("resolved twice: %v", err)
	}
	r = with(lang("", "pses"), map[string]Extractor{"pses": lsp(nil)}).Resolved().Resolved()
	if err := r.Validate(); err != nil {
		t.Errorf("new style resolved twice: %v", err)
	}
}

// The legacy language shape with a JSON argument in args loads, validates and
// keeps the argument as written (a brace group that is not a placeholder).
func TestLegacyCommandWithBracesInArgs(t *testing.T) {
	const doc = `{"languages":{"go":{"command":"gopls","args":["-remote=auto","--config={\"a\":1}"],"extensions":[".go"]}}}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _, err := Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	r := c.Resolved()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	nes := r.ExtractorsFor("go")
	if len(nes) != 1 || !reflect.DeepEqual(nes[0].Argv, []string{"gopls", "-remote=auto", `--config={"a":1}`}) {
		t.Errorf("extractors = %+v", nes)
	}
}
