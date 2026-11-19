package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wamra/code-index/internal/config"
)

func tree(t *testing.T, files map[string]string) string {
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func cfgFor(ignore []string, max int64) config.Config {
	return config.Config{
		Languages: map[string]config.Language{"sh": {Command: "x", Extensions: []string{".sh"}}},
		Index:     config.Index{Ignore: ignore, MaxFileSize: max},
	}.Resolved()
}

func rels(c []candidate) string {
	var out []string
	for _, x := range c {
		out = append(out, x.rel)
	}
	return strings.Join(out, ",")
}

func TestWalkRespectsGitignoreAndGlobs(t *testing.T) {
	root := tree(t, map[string]string{
		".gitignore":        "generated/\n*.skip.sh\n",
		"a.sh":              "a",
		"notes.txt":         "not a configured language",
		"generated/g.sh":    "ignored by .gitignore",
		"x.skip.sh":         "ignored by pattern",
		"sub/.gitignore":    "local.sh\n!keep.sh\n",
		"sub/local.sh":      "ignored by nested .gitignore",
		"sub/keep.sh":       "kept",
		"local.sh":          "nested rule does not apply here",
		"node_modules/n.sh": "ignored by index.ignore default",
		"docs/old/o.sh":     "ignored by config glob",
		".git/hooks/h.sh":   "never indexed",
	})
	got, _, err := walk(root, cfgFor([]string{"node_modules", "docs/old"}, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if want := "a.sh,local.sh,sub/keep.sh"; rels(got) != want {
		t.Errorf("walk = %s, want %s", rels(got), want)
	}
}

func TestWalkSizeLimitAndSymlinks(t *testing.T) {
	root := tree(t, map[string]string{"small.sh": "x", "big.sh": strings.Repeat("y", 100)})
	os.Symlink(filepath.Join(root, "small.sh"), filepath.Join(root, "link.sh"))
	got, st, err := walk(root, cfgFor(nil, 50))
	if err != nil {
		t.Fatal(err)
	}
	if rels(got) != "small.sh" || st.TooLarge != 1 {
		t.Errorf("walk = %s tooLarge=%d (symlinks must be skipped)", rels(got), st.TooLarge)
	}
}
