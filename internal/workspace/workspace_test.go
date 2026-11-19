package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIDAndDBPath(t *testing.T) {
	root := t.TempDir()
	id := ID(root)
	if len(id) != 16 {
		t.Fatalf("id %q", id)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if ID(link) != id {
		t.Error("a symlink to the root must give the same id (real path)")
	}
	if ID(t.TempDir()) == id {
		t.Error("different roots must differ")
	}
	env := func(k string) string {
		if k == "CODE_INDEX_DB" {
			return "/custom/db.sqlite"
		}
		return ""
	}
	if got := DBPath(root, "/home/u", env); got != "/custom/db.sqlite" {
		t.Errorf("CODE_INDEX_DB ignored: %s", got)
	}
	got := DBPath(root, "/home/u", func(string) string { return "" })
	if filepath.Base(got) != "index.sqlite" || filepath.Base(filepath.Dir(got)) != id {
		t.Errorf("db path = %s", got)
	}
}

func TestFindWalksUpToIndexedAncestor(t *testing.T) {
	home := t.TempDir()
	cache := t.TempDir()
	env := func(k string) string {
		if k == "XDG_CACHE_HOME" {
			return cache
		}
		return ""
	}
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	root, _ = filepath.EvalSymlinks(root)
	sub = filepath.Join(root, "a", "b")
	if got := Find(sub, home, env); got != sub {
		t.Errorf("without an index, Find = %s, want the directory itself", got)
	}
	db := DBPath(root, home, env)
	os.MkdirAll(filepath.Dir(db), 0o755)
	os.WriteFile(db, nil, 0o644)
	if got := Find(sub, home, env); got != root {
		t.Errorf("Find = %s, want %s", got, root)
	}
}
