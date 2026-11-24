package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// layeredSetup writes a central file (@ROOT@ is replaced by a new repo root's
// path) and, when repo is not empty, that root's .Wammy/config.json.
func layeredSetup(t *testing.T, central, repo string) (centralPath, root string) {
	t.Helper()
	root = t.TempDir()
	return layeredAt(t, root, central, repo)
}

func layeredAt(t *testing.T, root, central, repo string) (string, string) {
	t.Helper()
	q, _ := json.Marshal(root)
	central = strings.ReplaceAll(central, "@ROOT@", string(q[1:len(q)-1]))
	cp := filepath.Join(t.TempDir(), "central.json")
	if err := os.WriteFile(cp, []byte(central), 0o644); err != nil {
		t.Fatal(err)
	}
	if repo != "" {
		os.MkdirAll(filepath.Join(root, ".Wammy"), 0o700)
		if err := os.WriteFile(RepoPath(root), []byte(repo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cp, root
}

func joined(notes []string) string { return strings.Join(notes, "\n") }

func TestRepoFileCannotTrustItself(t *testing.T) {
	const repo = `{"trust_repo_config":true,"repos":{"@ROOT@":{"trust_repo_config":true,"extractors":{"x":{"type":"exec","argv":["evil"]}}}},
		"languages":{"a":{"extensions":[".a"],"extractor":"x"}},
		"extractors":{"x":{"type":"exec","argv":["evil"]}}}`
	for name, central := range map[string]string{
		"no central entry":   `{}`,
		"entry not trusted":  `{"repos":{"@ROOT@":{"trust_repo_config":false}}}`,
		"other repo trusted": `{"repos":{"/somewhere/else":{"trust_repo_config":true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			q, _ := json.Marshal(root)
			cp, _ := layeredAt(t, root, central, strings.ReplaceAll(repo, "@ROOT@", string(q[1:len(q)-1])))
			c, src, notes, err := LoadLayered(cp, root, env(nil))
			if err != nil {
				t.Fatal(err)
			}
			if c.TrustRepoConfig || len(c.Extractors) != 0 {
				t.Errorf("the repo file trusted itself: %+v", c)
			}
			if len(c.Repos) > 1 || c.Repos[root].TrustRepoConfig || len(c.Repos[root].Extractors) != 0 {
				t.Errorf("the repo file changed the repos entries: %+v", c.Repos)
			}
			if src["extractors.x"] != "" {
				t.Errorf("source of the dropped extractor: %v", src)
			}
			all := joined(notes)
			for _, want := range []string{"ignored repos", "ignored trust_repo_config", "ignored extractors.x"} {
				if !strings.Contains(all, want) {
					t.Errorf("notes lack %q:\n%s", want, all)
				}
			}
			// the dropped definition leaves a language naming nothing
			if err := c.Resolved().Validate(); err == nil || !strings.Contains(err.Error(), `unknown extractor "x"`) || !strings.Contains(err.Error(), "trust_repo_config") {
				t.Errorf("Validate = %v", err)
			}
		})
	}
	// a trusted repo's file still cannot add repos or flip trust
	cp, root := layeredSetup(t, `{"repos":{"@ROOT@":{"trust_repo_config":true}}}`, `{"repos":{"/other":{"trust_repo_config":true}},"trust_repo_config":false}`)
	c, _, notes, err := LoadLayered(cp, root, env(nil))
	if err != nil || !c.TrustRepoConfig || len(c.Repos) != 1 || len(notes) != 2 {
		t.Errorf("trusted repo: err=%v trust=%v repos=%v notes=%v", err, c.TrustRepoConfig, c.Repos, notes)
	}
}

func TestRepoPlacementErrors(t *testing.T) {
	cases := []struct{ name, central, want string }{
		{"nested repos", `{"repos":{"/a":{"repos":{"/b":{}}}}}`, `repos."/a".repos is not allowed`},
		{"nested empty repos", `{"repos":{"/a":{"repos":{}}}}`, `repos."/a".repos is not allowed`},
		{"top-level trust", `{"trust_repo_config":true}`, "trust_repo_config is only allowed inside a repos entry"},
		{"top-level trust false", `{"trust_repo_config":false}`, "trust_repo_config is only allowed inside a repos entry"},
		{"relative key", `{"repos":{"proj":{}}}`, `repos."proj": the key must be an absolute path`},
		{"same directory", `{"repos":{"/a/b":{},"/a/b/../b/":{}}}`, "are the same directory"},
	}
	for _, c := range cases {
		cp, root := layeredSetup(t, c.central, "")
		_, _, _, err := LoadLayered(cp, root, env(nil))
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), cp) {
			t.Errorf("%s: err = %v, want %q naming %s", c.name, err, c.want, cp)
		}
	}
	// Validate catches the same misplaced keys on a Config, matching or not
	bad := Config{Repos: map[string]Config{"/a": {Repos: map[string]Config{"/b": {}}}}}
	if err := bad.Resolved().Validate(); err == nil || !strings.Contains(err.Error(), "repos") {
		t.Errorf("nested repos in Validate: %v", err)
	}
	bad = Config{Repos: map[string]Config{"/a": {Extractors: map[string]Extractor{"x": {Type: "exec"}}}}}
	if err := bad.Resolved().Validate(); err == nil || !strings.Contains(err.Error(), `repos."/a".extractors.x`) {
		t.Errorf("bad extractor in a repos entry: %v", err)
	}
	// a Config whose nested repos entry parsed from a file is caught before that
	if _, _, _, err := LoadLayered(write(t, `{"repos":{"/a":{"nope":1}}}`), t.TempDir(), env(nil)); err == nil {
		t.Error("unknown key in a repos entry must be an error")
	}
}

const centralShell = `"languages":{"sh":{"command":"central-ls","args":["serve"],"extensions":[".sh"],"startup_timeout_s":7}},
	"embed":{"provider":"ollama","model":"central-model"}`

const evilRepo = `{"index":{"max_file_size":5},
	"embed":{"provider":"openai","base_url":"http://evil","model":"m"},
	"summarize":{"provider":"openai","base_url":"http://evil","model":"m"},
	"extractors":{"mine":{"type":"exec","argv":["evil"]}},
	"languages":{
		"sh":{"command":"evil-ls","args":["x"],"env":{"A":"b"},"extensions":[".sh",".bash"],"extractor":"powershell-ast","request_timeout_s":9},
		"new":{"command":"evil","extensions":[".n"],"extractor":"mine"}}}`

func TestUntrustedRepoFileIsGated(t *testing.T) {
	cp, root := layeredSetup(t, `{`+centralShell+`}`, evilRepo)
	r := RepoPath(root)
	c, src, notes, err := LoadLayered(cp, root, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	fix := `define it in ` + cp + ` under repos."` + root + `", or set repos."` + root + `".trust_repo_config to true`
	for _, want := range []string{
		r + ": ignored embed (a repo file cannot choose where source is sent unless trusted); " + fix,
		r + ": ignored summarize (",
		r + ": ignored extractors.mine (a repo file cannot define commands unless trusted); " + fix,
		r + ": ignored languages.sh.command (", r + ": ignored languages.sh.args (", r + ": ignored languages.sh.env (",
		r + ": ignored languages.new.command (",
	} {
		if !strings.Contains(joined(notes), want) {
			t.Errorf("notes lack %q:\n%s", want, joined(notes))
		}
	}
	if len(notes) != 7 {
		t.Errorf("want 7 notes, got %d:\n%s", len(notes), joined(notes))
	}
	if c.Index.MaxFileSize != 5 || src["index"] != r {
		t.Errorf("index must still apply: %+v %v", c.Index, src)
	}
	if c.Embed.Model != "central-model" || src["embed"] != cp || c.Summarize.BaseURL != "" {
		t.Errorf("embed/summarize must stay central: %+v %+v %v", c.Embed, c.Summarize, src)
	}
	sh := c.Languages["sh"]
	if sh.Command != "central-ls" || len(sh.Args) != 1 || sh.Args[0] != "serve" || sh.Env != nil {
		t.Errorf("command/args/env must stay central: %+v", sh)
	}
	if len(sh.Extensions) != 2 || sh.Extractor != "powershell-ast" || sh.StartupTimeoutS != 7 || sh.RequestTimeoutS != 9 || src["languages.sh"] != r {
		t.Errorf("allowed fields must overlay field by field: %+v %v", sh, src)
	}
	nl := c.Languages["new"]
	if nl.Command != "" || nl.Extractor != "mine" || len(c.Extractors) != 0 {
		t.Errorf("new language from allowed fields only: %+v extractors %v", nl, c.Extractors)
	}
	// the name that points at the dropped definition carries the same fix
	err = c.Resolved().Validate()
	if err == nil || !strings.Contains(err.Error(), `unknown extractor "mine"`) || !strings.Contains(err.Error(), fix) {
		t.Errorf("Validate = %v", err)
	}
	// without that language the rest is valid; sh keeps its central server first
	delete(c.Languages, "new")
	res := c.Resolved()
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	if names := res.Languages["sh"].Extractor.Names(); strings.Join(names, ",") != "sh-lsp,powershell-ast" {
		t.Errorf("sh extractors = %v", names)
	}
	// a dropped command with no extractor name says why too
	cp, root = layeredSetup(t, `{}`, `{"languages":{"x":{"command":"evil","extensions":[".x"]}}}`)
	c, _, _, _ = LoadLayered(cp, root, env(nil))
	if err := c.Resolved().Validate(); err == nil || !strings.Contains(err.Error(), "extractor is required") || !strings.Contains(err.Error(), "ignored languages.x.command") {
		t.Errorf("Validate = %v", err)
	}
}

func TestTrustedRepoFileKeepsFullBehaviour(t *testing.T) {
	cp, root := layeredSetup(t, `{`+centralShell+`,"repos":{"@ROOT@":{"trust_repo_config":true}}}`, evilRepo)
	c, src, notes, err := LoadLayered(cp, root, env(nil))
	if err != nil || len(notes) != 0 {
		t.Fatalf("err=%v notes=%v", err, notes)
	}
	if c.Embed.Model != "m" || c.Embed.BaseURL != "http://evil" || c.Extractors["mine"].Argv[0] != "evil" ||
		c.Languages["sh"].Command != "evil-ls" || c.Languages["new"].Command != "evil" || !c.TrustRepoConfig {
		t.Errorf("trusted repo file not applied: %+v", c)
	}
	if err := c.Resolved().Validate(); err != nil {
		t.Error(err)
	}
	if src["repos"] != cp+"#repos."+root {
		t.Errorf("source[repos] = %q", src["repos"])
	}
}

func TestCentralReposEntry(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	central := `{"languages":{"top":{"extensions":[".t"],"extractor":"powershell-ast"}},
		"repos":{"@ROOT@":{"languages":{"mine":{"extensions":[".m"],"command":"my-ls"}},
			"extractors":{"e":{"type":"exec","argv":["tool"]}},"index":{"max_file_size":3},"embed":{"model":"repo-model"}}}}`
	cp, _ := layeredAt(t, root, central, "")
	c, src, notes, err := LoadLayered(cp, root, env(nil))
	if err != nil || len(notes) != 0 {
		t.Fatalf("err=%v notes=%v", err, notes)
	}
	entry := cp + "#repos." + root
	if c.Languages["mine"].Command != "my-ls" || c.Extractors["e"].Argv[0] != "tool" || c.Index.MaxFileSize != 3 || c.Embed.Model != "repo-model" || c.Languages["top"].Extensions == nil {
		t.Errorf("entry not applied over the top level: %+v", c)
	}
	for k, want := range map[string]string{"languages.mine": entry, "extractors.e": entry, "index": entry, "embed": entry, "languages.top": cp, "repos": entry} {
		if src[k] != want {
			t.Errorf("source[%s] = %q, want %q", k, src[k], want)
		}
	}
	if c.TrustRepoConfig {
		t.Error("an entry without trust_repo_config is not trusted")
	}
	// another repo sees the top level only
	c, src, _, err = LoadLayered(cp, other, env(nil))
	if err != nil || c.Languages["mine"].Command != "" || len(c.Extractors) != 0 || c.Index.MaxFileSize != 0 || src["repos"] != "" {
		t.Errorf("entry leaked to another repo: err=%v %+v %v", err, c, src)
	}
	// Effective: only the merged view of this repo, with the trust state
	c, src, _, _ = LoadLayered(cp, root, env(nil))
	eff := string(Effective(c, src, env(nil)))
	var top map[string]json.RawMessage
	json.Unmarshal([]byte(eff), &top)
	if _, dumped := top["repos"]; dumped || !strings.Contains(eff, `"_repo_config": "untrusted: `+entry) {
		t.Errorf("Effective:\n%s", eff)
	}
}

func TestCentralReposKeyMatching(t *testing.T) {
	check := func(t *testing.T, key, root string, want bool) {
		t.Helper()
		kq, _ := json.Marshal(key)
		cp := write(t, `{"repos":{`+string(kq)+`:{"index":{"max_file_size":3}}}}`)
		c, src, _, err := LoadLayered(cp, root, env(nil))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Index.MaxFileSize == 3 && src["repos"] != ""; got != want {
			t.Errorf("key %q root %q: matched = %v, want %v", key, root, got, want)
		}
	}
	base := t.TempDir() // behind /var -> /private/var on macOS
	real, _ := filepath.EvalSymlinks(base)
	t.Run("temp dir resolved either way", func(t *testing.T) {
		check(t, base, real, true)
		check(t, real, base, true)
		check(t, base+string(filepath.Separator), real, true)
		check(t, filepath.Join(base, "x", ".."), real, true)
	})
	t.Run("symlink", func(t *testing.T) {
		dir := filepath.Join(real, "repo")
		link := filepath.Join(real, "link")
		os.Mkdir(dir, 0o755)
		if err := os.Symlink(dir, link); err != nil {
			t.Skip("no symlinks:", err)
		}
		check(t, link, dir, true)
		check(t, dir, link, true)
		check(t, link, filepath.Join(real, "elsewhere"), false)
	})
	t.Run("not a prefix match", func(t *testing.T) {
		os.Mkdir(filepath.Join(real, "sub"), 0o755)
		check(t, real, filepath.Join(real, "sub"), false)
	})
	t.Run("home", func(t *testing.T) {
		t.Setenv("HOME", real)
		t.Setenv("USERPROFILE", real)
		proj := filepath.Join(real, "proj")
		os.Mkdir(proj, 0o755)
		check(t, "~/proj", proj, true)
		check(t, "~/proj", filepath.Join(real, "other"), false)
	})
	t.Run("missing path falls back to the cleaned path", func(t *testing.T) {
		gone := filepath.Join(real, "gone", "repo")
		check(t, gone, gone, true)
	})
	t.Run("tmp vs private tmp", func(t *testing.T) {
		if runtime.GOOS != "darwin" {
			t.Skip("macOS only")
		}
		d, err := os.MkdirTemp("/private/tmp", "ci-keymatch-")
		if err != nil {
			t.Skip(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		check(t, "/tmp/"+filepath.Base(d), d, true)
		check(t, d, "/tmp/"+filepath.Base(d), true)
	})
	t.Run("two spellings of one directory", func(t *testing.T) {
		kq, _ := json.Marshal(base)
		rq, _ := json.Marshal(real)
		if base == real {
			t.Skip("temp dir is not behind a symlink")
		}
		cp := write(t, `{"repos":{`+string(kq)+`:{},`+string(rq)+`:{}}}`)
		if _, _, _, err := LoadLayered(cp, real, env(nil)); err == nil || !strings.Contains(err.Error(), "same directory") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestCentralOnlyAndRepoOnly(t *testing.T) {
	// central only: everything in the top level, no repo file
	cp, root := layeredSetup(t, `{`+centralShell+`,"extractors":{"e":{"type":"exec","argv":["t"]}}}`, "")
	c, _, notes, err := LoadLayered(cp, root, env(nil))
	if err != nil || len(notes) != 0 || c.Resolved().Validate() != nil || c.Languages["sh"].Command != "central-ls" {
		t.Errorf("central only: err=%v notes=%v", err, notes)
	}
	// repo file only (an untrusted one): extensions + built-in extractor name + index work
	missing := filepath.Join(t.TempDir(), "none.json")
	root = t.TempDir()
	os.MkdirAll(filepath.Join(root, ".Wammy"), 0o700)
	os.WriteFile(RepoPath(root), []byte(`{"languages":{"ps":{"extensions":[".ps1"],"extractor":"powershell-ast"}},"index":{"max_file_size":9},"embed":{"model":"m"}}`), 0o644)
	var src Sources
	c, src, notes, err = LoadLayered(missing, root, env(nil))
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "ignored embed") || !strings.Contains(notes[0], missing) {
		t.Fatalf("repo only: err=%v notes=%v", err, notes)
	}
	if err := c.Resolved().Validate(); err != nil || c.Index.MaxFileSize != 9 || src["languages.ps"] != RepoPath(root) || c.EmbedEnabled() {
		t.Errorf("repo only: %v %+v", err, c)
	}
}
