package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/embedtest"
	"github.com/wamra/code-index/internal/lsptest"
)

func TestMain(m *testing.M) {
	if os.Getenv("CODE_INDEX_FAKE_LSP_SERVE") == "1" {
		lsptest.Main()
		return
	}
	os.Exit(m.Run())
}

func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := run(args, &out, &errw)
	return code, out.String(), errw.String()
}

func mustOK(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errw := cli(t, args...)
	if code != 0 {
		t.Fatalf("code-index %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out, errw)
	}
	return out
}

type env struct {
	root, cfgPath, dbPath string
}

// setup writes a config with a fake language server (and optionally a fake
// Ollama), points the tool at a temp database and copies the PowerShell fixture.
func setup(t *testing.T, embedURL string) env {
	t.Helper()
	tmp := t.TempDir()
	e := env{root: filepath.Join(tmp, "ws"), cfgPath: filepath.Join(tmp, "config.json"), dbPath: filepath.Join(tmp, "db", "index.sqlite")}
	src := "../../testdata/fixture-ps"
	filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(e.root, rel), 0o755)
		}
		b, _ := os.ReadFile(p)
		return os.WriteFile(filepath.Join(e.root, rel), b, 0o644)
	})
	e.root, _ = filepath.EvalSymlinks(e.root)
	cfg := map[string]any{
		"languages": map[string]any{
			"powershell": map[string]any{
				"command": os.Args[0], "args": []string{"-test.run=^$"},
				"env":        map[string]string{"CODE_INDEX_FAKE_LSP_SERVE": "1", "CODE_INDEX_FAKE_LSP": "emptyfirst=1"},
				"extensions": []string{".ps1", ".psm1", ".psd1"}, "language_id": "powershell",
				"startup_timeout_s": 20, "request_timeout_s": 10,
			},
		},
	}
	if embedURL != "" {
		cfg["embed"] = map[string]any{"provider": "ollama", "base_url": embedURL, "model": "fake", "dims": 64, "doc_prefix": "", "query_prefix": ""}
	}
	b, _ := json.Marshal(cfg)
	os.WriteFile(e.cfgPath, b, 0o644)
	t.Setenv("CODE_INDEX_CONFIG", e.cfgPath)
	t.Setenv("CODE_INDEX_DB", e.dbPath)
	t.Setenv("HOME", tmp)
	t.Chdir(tmp)
	return e
}

func TestEndToEndWithoutEmbedder(t *testing.T) {
	e := setup(t, "")

	// before any index: a clear error, not a crash
	if code, _, errw := cli(t, "search", "x"); code != 1 || !strings.Contains(errw, "no index") {
		t.Fatalf("search before index: %d %q", code, errw)
	}

	out := mustOK(t, "index", e.root, "--quiet")
	if !strings.Contains(out, "5 file(s) of configured languages; indexed 5") || !strings.Contains(out, "failed 0") {
		t.Fatalf("index summary: %s", out)
	}

	// definition ranks first
	out = mustOK(t, "search", "Get-Widget")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.Contains(lines[0], "function Get-Widget  Widgets/Widgets.psm1:6") || !strings.HasSuffix(lines[len(lines)-1], "route: name") {
		t.Fatalf("search output:\n%s", out)
	}
	if !strings.Contains(lines[1], "function Get-Widget") {
		t.Errorf("signature line missing:\n%s", out)
	}

	// JSON
	var res struct {
		Hits  []map[string]any `json:"hits"`
		Route string           `json:"route"`
	}
	if err := json.Unmarshal([]byte(mustOK(t, "search", "Get-Widget", "--json", "--k", "2")), &res); err != nil {
		t.Fatal(err)
	}
	if res.Route != "name" || len(res.Hits) == 0 || res.Hits[0]["qualified_name"] != "Get-Widget" || res.Hits[0]["path"] != "Widgets/Widgets.psm1" {
		t.Errorf("json = %+v", res)
	}
	// no hits: valid JSON with an empty list
	if err := json.Unmarshal([]byte(mustOK(t, "search", "zzzqqq", "--json", "--mode", "fts")), &res); err != nil || len(res.Hits) != 0 {
		t.Errorf("empty json: %v %+v", err, res)
	}

	// flags after the query, filters
	out = mustOK(t, "search", "widget", "--kind", "function", "--lang", "powershell", "--path", "Widgets/", "--k", "5")
	if !strings.Contains(out, "New-Widget") || strings.Contains(out, "lib.ps1") {
		t.Errorf("filtered search:\n%s", out)
	}

	// vector mode without an embedder is an error with a way out
	if code, _, errw := cli(t, "search", "x", "--mode", "vector"); code != 1 || !strings.Contains(errw, "--mode fts") {
		t.Errorf("vector without embedder: %d %q", code, errw)
	}
	if code, _, errw := cli(t, "index", "--embed", e.root, "--quiet"); code != 1 || !strings.Contains(errw, "no embedder configured") {
		t.Errorf("--embed without embedder: %d %q", code, errw)
	}

	// show: name, qualified name, path:line, absolute path:line
	out = mustOK(t, "show", "Get-Widget", "--source")
	for _, want := range []string{"function Get-Widget", "file:      Widgets/Widgets.psm1:6-16 (powershell)", "signature: function Get-Widget", "doc:       Returns a widget by name.", "---\nfunction Get-Widget {"} {
		if !strings.Contains(out, want) {
			t.Errorf("show output lacks %q:\n%s", want, out)
		}
	}
	if got := mustOK(t, "show", "Widgets/Widgets.psm1:8"); !strings.Contains(got, "function Get-Widget") {
		t.Errorf("show path:line:\n%s", got)
	}
	if got := mustOK(t, "show", filepath.Join(e.root, "lib.ps1")+":4"); !strings.Contains(got, "function Invoke-Helper") {
		t.Errorf("show absolute path:line:\n%s", got)
	}
	if code, _, errw := cli(t, "show", "No-Such-Thing"); code != 1 || !strings.Contains(errw, "no symbol matches") {
		t.Errorf("show unknown: %d %q", code, errw)
	}
	var sh struct {
		Name      string `json:"name"`
		StartLine int    `json:"start_line"`
	}
	json.Unmarshal([]byte(mustOK(t, "show", "get-widget", "--json")), &sh) // case-insensitive tier
	if sh.Name != "Get-Widget" || sh.StartLine != 6 {
		t.Errorf("show --json = %+v", sh)
	}

	// ambiguity lists the candidates
	os.WriteFile(filepath.Join(e.root, "other.ps1"), []byte("function Get-Widget {\n}\n"), 0o644)
	mustOK(t, "sweep", "--quiet")
	code, _, errw := cli(t, "show", "Get-Widget")
	if code != 1 || !strings.Contains(errw, "ambiguous (2 candidates)") || !strings.Contains(errw, "other.ps1:1") || !strings.Contains(errw, "Widgets/Widgets.psm1:6") {
		t.Errorf("ambiguous show: %d %q", code, errw)
	}

	// stats and doctor
	out = mustOK(t, "stats")
	for _, want := range []string{"workspace:   " + e.root, "files:       6 (ok 6)", "powershell 6", "embedder:    none recorded"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats lacks %q:\n%s", want, out)
		}
	}
	out = mustOK(t, "doctor")
	for _, want := range []string{"ok    chunk_fts", "ok    name_fts", "ok    fts rows", "ok    freshness", "not configured: search uses names and full text only"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}

	// sweep: edit + delete
	os.Remove(filepath.Join(e.root, "other.ps1"))
	os.WriteFile(filepath.Join(e.root, "lib.ps1"), []byte("function Invoke-Edited {\n}\n"), 0o644)
	out = mustOK(t, "sweep", "--quiet")
	if !strings.Contains(out, "indexed 1 (") || !strings.Contains(out, "deleted 1") {
		t.Errorf("sweep summary: %s", out)
	}
	if out := mustOK(t, "search", "Invoke-Edited"); !strings.Contains(out, "function Invoke-Edited") {
		t.Errorf("edited symbol not searchable:\n%s", out)
	}
	// doctor reports drift
	os.WriteFile(filepath.Join(e.root, "lib.ps1"), []byte("function Invoke-Again {\n}\n"), 0o644)
	if out := mustOK(t, "doctor"); !strings.Contains(out, "WARN  freshness") || !strings.Contains(out, "1 file(s) modified") {
		t.Errorf("doctor freshness:\n%s", out)
	}
	if out := mustOK(t, "search", "Invoke-Edited"); !strings.Contains(out, "[stale: modified]") {
		t.Errorf("stale marker missing:\n%s", out)
	}
}

func TestEndToEndWithEmbedder(t *testing.T) {
	srv := embedtest.New(64, "fake")
	defer srv.Close()
	e := setup(t, srv.URL)

	out := mustOK(t, "index", e.root, "--embed", "--quiet")
	if !regexp.MustCompile(`embedded \d+ chunk\(s\) with ollama\|fake\|64\|\|v1`).MatchString(out) || strings.Contains(out, "embedded 0 ") {
		t.Fatalf("index --embed output: %s", out)
	}
	out = mustOK(t, "search", "function that returns a widget by name", "--k", "3")
	if !strings.HasSuffix(strings.TrimSpace(out), "route: vector") || !strings.Contains(strings.Split(out, "\n")[0], "function Get-Widget") {
		t.Fatalf("vector search:\n%s", out)
	}
	out = mustOK(t, "stats")
	if !strings.Contains(out, "pending 0, failed 0") || !strings.Contains(out, "ollama|fake|64||v1 (64 dims)") {
		t.Errorf("stats:\n%s", out)
	}
	out = mustOK(t, "doctor")
	if !strings.Contains(out, "ok    embedder") || !strings.Contains(out, "no chunks pending") {
		t.Errorf("doctor:\n%s", out)
	}

	// unreachable provider: search degrades to full text with a warning, exit 0
	srv.Down.Store(true)
	code, out, errw := cli(t, "search", "function that returns a widget by name")
	if code != 0 || !strings.Contains(errw, "using full-text search") || !strings.HasSuffix(strings.TrimSpace(out), "route: fts") {
		t.Errorf("provider down: %d\n%s\n%s", code, out, errw)
	}
	if code, _, errw := cli(t, "search", "x", "--mode", "vector"); code != 1 || !strings.Contains(errw, "unreachable") {
		t.Errorf("vector while down: %d %q", code, errw)
	}
}

func TestConfigCommand(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("PATH", tmp) // no language servers on PATH
	t.Setenv("CODE_INDEX_CONFIG", filepath.Join(tmp, "cfg", "config.json"))
	t.Setenv("MY_SECRET_KEY", "super-secret-value")

	out := mustOK(t, "config")
	if !strings.Contains(out, "not found: defaults and environment only") || !strings.Contains(out, `"enabled": false`) {
		t.Errorf("config without file:\n%s", out)
	}
	out = mustOK(t, "config", "init")
	if !strings.Contains(out, "wrote ") || !strings.Contains(out, "no known language server found on PATH") {
		t.Errorf("config init: %s", out)
	}
	if code, _, errw := cli(t, "config", "init"); code != 1 || !strings.Contains(errw, "exists") {
		t.Errorf("second init: %d %q", code, errw)
	}
	mustOK(t, "config", "init", "--force")
	body, _ := os.ReadFile(os.Getenv("CODE_INDEX_CONFIG"))
	os.WriteFile(os.Getenv("CODE_INDEX_CONFIG"), []byte(strings.Replace(string(body), `"api_key_env": ""`, `"api_key_env": "MY_SECRET_KEY"`, 1)), 0o644)
	out = mustOK(t, "config")
	if strings.Contains(out, "super-secret-value") || !strings.Contains(out, "MY_SECRET_KEY") || !strings.Contains(out, `"_embed_api_key": "set"`) {
		t.Errorf("effective config must name the key variable only:\n%s", out)
	}
	// a broken file is reported
	os.WriteFile(os.Getenv("CODE_INDEX_CONFIG"), []byte(`{"languages":{"x":{"command":"c"}}}`), 0o644)
	if code, _, errw := cli(t, "config"); code != 1 || !strings.Contains(errw, "languages.x.extensions") {
		t.Errorf("invalid config: %d %q", code, errw)
	}
}

func TestUsageAndVersion(t *testing.T) {
	if code, _, errw := cli(t); code != 2 || !strings.Contains(errw, "usage: code-index") {
		t.Errorf("no args: %d %q", code, errw)
	}
	if code, _, errw := cli(t, "bogus"); code != 2 || !strings.Contains(errw, `unknown command "bogus"`) {
		t.Errorf("unknown: %d %q", code, errw)
	}
	if out := mustOK(t, "version"); !strings.HasPrefix(out, "code-index ") {
		t.Errorf("version: %q", out)
	}
	for _, c := range []string{"index", "sweep", "search", "show", "stats", "doctor", "config", "version"} {
		if _, ok := commands[c]; !ok {
			t.Errorf("command %s missing", c)
		}
	}
}
