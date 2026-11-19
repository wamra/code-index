package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/provider"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestPathHonorsEnv(t *testing.T) {
	if got := Path("/h", env(nil)); got != filepath.Join("/h", ".config", "code-index", "config.json") {
		t.Errorf("default path = %s", got)
	}
	if got := Path("/h", env(map[string]string{"CODE_INDEX_CONFIG": "/x/c.json"})); got != "/x/c.json" {
		t.Errorf("env path = %s", got)
	}
}

func TestMissingFileMeansDefaultsAndEmbedOff(t *testing.T) {
	c, found, err := Load(filepath.Join(t.TempDir(), "none.json"), env(nil))
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	r := c.Resolved()
	if r.EmbedEnabled() {
		t.Error("embeddings must be off when nothing is configured")
	}
	if err := r.Validate(); err != nil {
		t.Error(err)
	}
	if r.Index.MaxFileSize != DefaultMaxFileSize || len(r.Index.Ignore) == 0 {
		t.Errorf("index defaults: %+v", r.Index)
	}
}

func write(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEmbedEnabledSemantics(t *testing.T) {
	cases := []struct {
		name, body string
		env        map[string]string
		want       bool
	}{
		{"provider given", `{"embed":{"provider":"ollama"}}`, nil, true},
		{"model given", `{"embed":{"model":"embeddinggemma"}}`, nil, true},
		{"explicit off wins", `{"embed":{"provider":"ollama","enabled":false}}`, nil, false},
		{"explicit on", `{"embed":{"enabled":true}}`, nil, true},
		{"env turns it on", `{}`, map[string]string{"CODE_INDEX_EMBED_MODEL": "nomic-embed-text"}, true},
	}
	for _, c := range cases {
		cfg, _, err := Load(write(t, c.body), env(c.env))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := cfg.Resolved().EmbedEnabled(); got != c.want {
			t.Errorf("%s: enabled = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEnvOverridesFile(t *testing.T) {
	p := write(t, `{"embed":{"provider":"ollama","model":"a"},"summarize":{"model":"s"}}`)
	c, _, _ := Load(p, env(map[string]string{
		"CODE_INDEX_EMBED_MODEL": "b", "CODE_INDEX_EMBED_BASE_URL": "http://h:1/", "CODE_INDEX_SUMMARIZE_MODEL": "t",
	}))
	r := c.Resolved()
	if r.Embed.Model != "b" || r.Embed.BaseURL != "http://h:1" || r.Summarize.Model != "t" {
		t.Errorf("got %+v / %+v", r.Embed, r.Summarize)
	}
}

func TestUnknownFieldAndBadJSON(t *testing.T) {
	if _, _, err := Load(write(t, `{"embd":{}}`), env(nil)); err == nil {
		t.Error("unknown field must be rejected")
	}
	if _, _, err := Load(write(t, `{`), env(nil)); err == nil {
		t.Error("bad JSON must be rejected")
	}
}

func TestLanguageValidation(t *testing.T) {
	bad := map[string]string{
		"no command":       `{"languages":{"x":{"extensions":[".x"]}}}`,
		"no extensions":    `{"languages":{"x":{"command":"c"}}}`,
		"extension no dot": `{"languages":{"x":{"command":"c","extensions":["x"]}}}`,
		"extension clash":  `{"languages":{"a":{"command":"c","extensions":[".x"]},"b":{"command":"c","extensions":[".X"]}}}`,
	}
	for name, body := range bad {
		c, _, err := Load(write(t, body), env(nil))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := c.Resolved().Validate(); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
	c, _, _ := Load(write(t, `{"languages":{"sh":{"command":"c","extensions":[".sh",".Bash"]}}}`), env(nil))
	r := c.Resolved()
	if n, ok := r.LanguageFor(".BASH"); !ok || n != "sh" {
		t.Errorf("LanguageFor = %q %v", n, ok)
	}
	if _, ok := r.LanguageFor(".py"); ok {
		t.Error(".py must not match")
	}
	if l := r.Languages["sh"]; l.LanguageID != "sh" || l.StartupTimeoutS != 120 || l.RequestTimeoutS != 30 {
		t.Errorf("defaults not applied: %+v", l)
	}
}

func TestInitRoundTrips(t *testing.T) {
	langs := map[string]Language{"bash": KnownServers[0].Lang}
	p := write(t, string(Init(langs)))
	c, found, err := Load(p, env(nil))
	if err != nil || !found {
		t.Fatalf("init output does not load: %v", err)
	}
	r := c.Resolved()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if r.EmbedEnabled() {
		t.Error("starter file must leave embeddings off")
	}
	if r.Embed.Provider != provider.Ollama || r.Embed.Model != provider.DefaultEmbedModel {
		t.Errorf("starter embed section: %+v", r.Embed)
	}
	if _, ok := r.Languages["bash"]; !ok {
		t.Error("bash entry lost")
	}
}

func TestDiscover(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, ".claude", "plugins", "cache", "mk", "powershell-lsp", "1.0", "scripts", "start-pses.ps1")
	os.MkdirAll(filepath.Dir(script), 0o755)
	os.WriteFile(script, []byte("#"), 0o644)
	look := func(name string) (string, error) {
		if name == "bash-language-server" || name == "pwsh" {
			return "/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	got := Discover(home, look)
	if len(got) != 2 || got["bash"].Command != "bash-language-server" {
		t.Fatalf("discovered %v", got)
	}
	ps := got["powershell"]
	if ps.Command != "pwsh" || ps.Args[len(ps.Args)-1] != script {
		t.Errorf("powershell entry: %+v", ps)
	}
	// no pwsh script installed -> no powershell entry
	if _, ok := Discover(t.TempDir(), look)["powershell"]; ok {
		t.Error("powershell must need the start script")
	}
}

func TestEffectiveNeverPrintsKeys(t *testing.T) {
	t.Setenv("MY_KEY", "sk-secret-value")
	c, _, _ := Load(write(t, `{"embed":{"provider":"openai","base_url":"http://x","model":"m","api_key_env":"MY_KEY"}}`), os.Getenv)
	out := string(Effective(c, Sources{"embed": "x"}, os.Getenv))
	if strings.Contains(out, "sk-secret-value") {
		t.Fatal("the key value leaked into the effective config")
	}
	if !strings.Contains(out, `"_embed_api_key": "set"`) || !strings.Contains(out, "MY_KEY") {
		t.Errorf("expected the variable name and its state:\n%s", out)
	}
}

func TestLayeredMerge(t *testing.T) {
	dir := t.TempDir()
	g := filepath.Join(dir, "g.json")
	r := filepath.Join(dir, "r.json")
	os.WriteFile(g, []byte(`{"embed":{"provider":"ollama","model":"gm"},"index":{"max_file_size":10},
		"languages":{"a":{"command":"ga","extensions":[".a"]},"b":{"command":"gb","extensions":[".b"]}}}`), 0o644)
	os.WriteFile(r, []byte(`{"embed":{"provider":"openai","model":"rm","base_url":"http://r"},
		"languages":{"b":{"command":"rb","extensions":[".b"]},"c":{"command":"rc","extensions":[".c"]}}}`), 0o644)
	c, src, err := LoadLayered(g, r, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	res := c.Resolved()
	if res.Embed.Provider != "openai" || res.Embed.Model != "rm" {
		t.Errorf("embed must be replaced as a whole: %+v", res.Embed)
	}
	if res.Index.MaxFileSize != 10 {
		t.Errorf("index from global: %+v", res.Index)
	}
	if res.Languages["a"].Command != "ga" || res.Languages["b"].Command != "rb" || res.Languages["c"].Command != "rc" {
		t.Errorf("languages merged per key: %+v", res.Languages)
	}
	want := Sources{"embed": r, "summarize": "default", "index": g, "languages.a": g, "languages.b": r, "languages.c": r}
	for k, v := range want {
		if src[k] != v {
			t.Errorf("source[%s] = %q, want %q", k, src[k], v)
		}
	}
	// a repo file without sections changes nothing; missing files are fine
	c2, src2, err := LoadLayered(g, filepath.Join(dir, "none.json"), env(nil))
	if err != nil || src2["embed"] != g || c2.Resolved().Embed.Model != "gm" {
		t.Errorf("missing repo file: %v %v", err, src2)
	}
	// a bad repo file is reported with its path
	os.WriteFile(r, []byte(`{"nope":1}`), 0o644)
	if _, _, err := LoadLayered(g, r, env(nil)); err == nil || !strings.Contains(err.Error(), r) {
		t.Errorf("bad repo file: %v", err)
	}
}
