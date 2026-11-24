package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/embedtest"
	"github.com/wamra/code-index/internal/extractest"
	"github.com/wamra/code-index/internal/lsptest"
)

func TestMain(m *testing.M) {
	if os.Getenv("CODE_INDEX_FAKE_LSP_SERVE") == "1" {
		lsptest.Main()
		return
	}
	if extractest.Main() {
		return
	}
	if os.Getenv("CODE_INDEX_TEST_CLI") == "1" { // the test binary as the CLI
		main()
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
func setup(t *testing.T, embedURL string) env { return setupOpts(t, embedURL, "emptyfirst=1") }

// setupOpts is setup with the fake language server's quirk flags (see lsptest.ParseOptions).
func setupOpts(t *testing.T, embedURL, fakeOpts string) env {
	t.Helper()
	tmp := t.TempDir()
	e := env{root: filepath.Join(tmp, "ws"), cfgPath: filepath.Join(tmp, "config.json")}
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
	e.dbPath = filepath.Join(e.root, ".Wammy", "code.sqlite")
	cfg := map[string]any{
		"languages": map[string]any{
			"powershell": map[string]any{
				"command": os.Args[0], "args": []string{"-test.run=^$"},
				"env":        map[string]string{"CODE_INDEX_FAKE_LSP_SERVE": "1", "CODE_INDEX_FAKE_LSP": fakeOpts},
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
	t.Setenv("CODE_INDEX_DB", "")
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("LocalAppData", filepath.Join(tmp, "localappdata"))
	t.Chdir(e.root)
	return e
}

func TestEndToEndWithoutEmbedder(t *testing.T) {
	e := setup(t, "")

	// before any index: a clear error, not a crash
	if code, _, errw := cli(t, "search", "x"); code != 1 || !strings.Contains(errw, "no index") || !strings.Contains(errw, "run `code-index index`") {
		t.Fatalf("search before index: %d %q", code, errw)
	}

	// doctor --no-index: config, language servers and embedder only, no index FAIL
	if out := mustOK(t, "doctor", "--no-index"); strings.Contains(out, "index ") || strings.Contains(out, "FAIL") || !strings.Contains(out, "ok    config") || !strings.Contains(out, "language powershell") || !strings.Contains(out, "embedder") {
		t.Errorf("doctor --no-index before any index:\n%s", out)
	}
	if code, out, _ := cli(t, "doctor"); code != 1 || !strings.Contains(out, "FAIL  index") {
		t.Errorf("plain doctor without an index should still fail on it: %d\n%s", code, out)
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
	for _, want := range []string{"workspace:   " + e.root, "database:    " + e.dbPath, "files:       6 (ok 6)", "powershell 6", "embedder:    none recorded"} {
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
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("PATH", tmp) // no language servers on PATH
	t.Setenv("CODE_INDEX_CONFIG", filepath.Join(tmp, "cfg", "config.json"))
	t.Setenv("MY_SECRET_KEY", "super-secret-value")
	t.Chdir(tmp)

	out := mustOK(t, "config")
	if !strings.Contains(out, `"embed": "default"`) || !strings.Contains(out, `"enabled": false`) {
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
	for _, c := range []string{"index", "sweep", "search", "show", "stats", "doctor", "config", "install-skill", "version"} {
		if _, ok := commands[c]; !ok {
			t.Errorf("command %s missing", c)
		}
	}
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestPerRepoDatabaseInGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	e := setup(t, "")
	gitRun(t, e.root, "init", "-q")
	os.WriteFile(filepath.Join(e.root, ".gitignore"), []byte("*.log\n"), 0o644)
	gitignoreBefore, _ := os.ReadFile(filepath.Join(e.root, ".gitignore"))

	// index from a subdirectory: the root is the directory with .git
	t.Chdir(filepath.Join(e.root, "Widgets"))
	out := mustOK(t, "index", "--quiet")
	if !strings.Contains(out, "added .Wammy/ to ") || !strings.Contains(out, "5 file(s) of configured languages; indexed 5") {
		t.Fatalf("index output: %s", out)
	}
	if _, err := os.Stat(e.dbPath); err != nil {
		t.Fatalf("database not at <root>/.Wammy/code.sqlite: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(e.root, ".Wammy")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf(".Wammy mode = %v, want 0700", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Join(e.root, ".Wammy"))
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), "-wal") || strings.HasSuffix(en.Name(), "-shm") {
			t.Errorf("%s left behind after the command exited", en.Name())
		}
	}
	if st := gitRun(t, e.root, "status", "--short"); strings.Contains(st, ".Wammy") {
		t.Errorf("git status lists .Wammy:\n%s", st)
	}
	if after, _ := os.ReadFile(filepath.Join(e.root, ".gitignore")); string(after) != string(gitignoreBefore) {
		t.Error(".gitignore was modified")
	}
	// the second run finds it already ignored and prints nothing about it
	if out := mustOK(t, "sweep", "--quiet"); strings.Contains(out, "added") {
		t.Errorf("sweep: %s", out)
	}
	if out := mustOK(t, "index", "--quiet"); strings.Contains(out, "added") {
		t.Errorf("second index: %s", out)
	}
	// search and show find the database from a subdirectory
	if out := mustOK(t, "search", "Get-Widget"); !strings.Contains(out, "function Get-Widget  Widgets/Widgets.psm1:6") {
		t.Errorf("search from subdirectory:\n%s", out)
	}
	if out := mustOK(t, "doctor"); !strings.Contains(out, "ok    git-ignore     .Wammy/ is ignored by git") || !strings.Contains(out, e.dbPath) {
		t.Errorf("doctor:\n%s", out)
	}
	// the index never contains its own directory
	if out := mustOK(t, "stats"); !strings.Contains(out, "files:       5 (ok 5)") {
		t.Errorf("stats:\n%s", out)
	}
}

func TestRepoConfigOverride(t *testing.T) {
	e := setup(t, "")
	os.MkdirAll(filepath.Join(e.root, ".Wammy"), 0o700)
	repoCfg := filepath.Join(e.root, ".Wammy", "config.json")
	os.WriteFile(repoCfg, []byte(`{"embed":{"provider":"ollama","model":"repo-model","enabled":false},"index":{"max_file_size":5}}`), 0o644)
	out := mustOK(t, "config")
	var c struct {
		Sources map[string]string `json:"_sources"`
		Embed   struct{ Model string }
		Index   struct {
			MaxFileSize int `json:"max_file_size"`
		}
		Languages map[string]any
	}
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatal(err)
	}
	if c.Sources["embed"] != repoCfg || c.Sources["index"] != repoCfg || c.Sources["summarize"] != "default" || c.Sources["languages.powershell"] != e.cfgPath {
		t.Errorf("sources = %v", c.Sources)
	}
	if c.Embed.Model != "repo-model" || c.Index.MaxFileSize != 5 || c.Languages["powershell"] == nil {
		t.Errorf("merged config: %+v", c)
	}
	// the override applies to indexing: every fixture file is larger than 5 bytes
	if out := mustOK(t, "index", "--quiet"); !strings.Contains(out, "0 file(s) of configured languages") || !strings.Contains(out, "skipped too large 5") {
		t.Errorf("index with max_file_size 5: %s", out)
	}
}

func TestInstallSkill(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	want := filepath.Join(tmp, ".claude", "skills", "code-search", "SKILL.md")
	if out := mustOK(t, "install-skill", "--dry-run"); !strings.Contains(out, want) {
		t.Errorf("dry run: %s", out)
	}
	if _, err := os.Stat(want); err == nil {
		t.Fatal("dry run wrote the file")
	}
	if out := mustOK(t, "install-skill"); !strings.Contains(out, "skill installed: "+want) {
		t.Errorf("install: %s", out)
	}
	b, err := os.ReadFile(want)
	if err != nil || !strings.HasPrefix(string(b), "---\nname: code-search\n") {
		t.Fatalf("skill file: %v %.60q", err, b)
	}
	// only documented commands: every code-index subcommand named in the skill exists
	for _, m := range regexp.MustCompile("code-index ([a-z-]+)").FindAllStringSubmatch(string(b), -1) {
		if _, ok := commands[m[1]]; !ok {
			t.Errorf("skill mentions unknown command %q", m[1])
		}
	}
	custom := filepath.Join(tmp, "elsewhere")
	mustOK(t, "install-skill", "--dir", custom)
	if _, err := os.Stat(filepath.Join(custom, "code-search", "SKILL.md")); err != nil {
		t.Error(err)
	}
}
