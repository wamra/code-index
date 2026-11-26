package indexer

import (
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/config"
)

// secretCfg makes every extension a candidate so only the secret patterns can
// keep a file out.
func secretCfg(idx config.Index) config.Config {
	exts := []string{".sh", ".env", ".pem", ".key", ".p12", ".pfx", ".kdbx", ".json", ".py", ".local", ".prod", ".npmrc", ".netrc"}
	idx.MaxFileSize = 1 << 20
	return config.Config{
		Languages: map[string]config.Language{"any": {Command: "x", Extensions: exts}},
		Index:     idx,
	}.Resolved()
}

var secretTree = map[string]string{
	"ok.sh":                 "ok",
	".env":                  "S=1",
	".env.local":            "S=1",
	"cfg/.env.prod":         "S=1",
	"server.pem":            "k",
	"deep/dir/tls.key":      "k",
	"id_rsa.sh":             "k",
	"id_ed25519.pub.sh":     "k",
	"cert.p12":              "k",
	"cert.pfx":              "k",
	".npmrc":                "t",
	".netrc":                "t",
	"credentials.json":      "t",
	"credentials":           "t",
	"vault.kdbx":            "t",
	".aws/config.sh":        "t",
	"home/.ssh/config.sh":   "t",
	"UPPER/TLS.KEY":         "k",
	"keep/credential_ok.sh": "name does not start with credentials",
}

func TestWalkSkipsSecretFilesByDefault(t *testing.T) {
	root := tree(t, secretTree)
	c, _, err := walk(root, secretCfg(config.Index{}))
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(c); got != "keep/credential_ok.sh,ok.sh" {
		t.Errorf("indexed = %s", got)
	}
}

// A .gitignore negation or an index.ignore negation cannot bring a secret back.
func TestSecretIgnoreSurvivesNegation(t *testing.T) {
	files := map[string]string{".gitignore": "!.env\n!*.pem\n", "a.sh": "a", ".env": "S=1", "x.pem": "k"}
	root := tree(t, files)
	c, _, _ := walk(root, secretCfg(config.Index{Ignore: []string{"!.env", "!*.pem"}}))
	if got := rels(c); got != "a.sh" {
		t.Errorf("indexed = %s", got)
	}
}

func TestSecretIgnoreOverrideFromConfig(t *testing.T) {
	root := tree(t, secretTree)
	none := []string{}
	c, _, _ := walk(root, secretCfg(config.Index{SecretIgnore: &none}))
	if !strings.Contains(rels(c), ".env") || !strings.Contains(rels(c), "server.pem") {
		t.Errorf("an empty secret_ignore must index everything: %s", rels(c))
	}
	only := []string{"*.pem"}
	c, _, _ = walk(root, secretCfg(config.Index{SecretIgnore: &only}))
	if strings.Contains(rels(c), "server.pem") || !strings.Contains(rels(c), ".env") {
		t.Errorf("custom list replaces the default: %s", rels(c))
	}
}
