package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func noenv(string) string { return "" }

func realTemp(t *testing.T) string {
	r, _ := filepath.EvalSymlinks(t.TempDir())
	return r
}

func TestRootAndDBPath(t *testing.T) {
	base := realTemp(t)
	sub := filepath.Join(base, "repo", "a", "b")
	os.MkdirAll(sub, 0o755)
	repo := filepath.Join(base, "repo")
	if got := Root(sub); got != sub {
		t.Errorf("no .git: Root = %s, want the directory itself", got)
	}
	os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	if got := Root(sub); got != repo {
		t.Errorf("Root = %s, want %s", got, repo)
	}
	// a .git FILE (worktree or submodule) also marks a root
	wt := filepath.Join(base, "wt")
	os.MkdirAll(filepath.Join(wt, "x"), 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /elsewhere\n"), 0o644)
	if got := Root(filepath.Join(wt, "x")); got != wt {
		t.Errorf("Root with .git file = %s, want %s", got, wt)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err == nil { // may need privileges on Windows
		if got := Root(link); got != repo {
			t.Errorf("symlink: %s", got)
		}
	}
	if got := DBPath(repo, noenv); got != filepath.Join(repo, ".Wammy", "code.sqlite") { // native separators on disk
		t.Errorf("db path = %s", got)
	}
	if got := DBPath(repo, func(k string) string {
		if k == "CODE_INDEX_DB" {
			return "/custom.sqlite"
		}
		return ""
	}); got != "/custom.sqlite" {
		t.Errorf("override = %s", got)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

func TestEnsureExcluded(t *testing.T) {
	repo := realTemp(t)
	gitInit(t, repo)
	os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o644)
	before, _ := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if ig, known := GitIgnored(repo); !known || ig {
		t.Fatalf("before: ignored=%v known=%v", ig, known)
	}
	p, err := EnsureExcluded(repo)
	if err != nil || p == "" {
		t.Fatalf("EnsureExcluded = %q %v", p, err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), ".Wammy/\n") {
		t.Errorf("exclude file: %q", b)
	}
	if after, _ := os.ReadFile(filepath.Join(repo, ".gitignore")); string(after) != string(before) {
		t.Error(".gitignore must not be touched")
	}
	if ig, _ := GitIgnored(repo); !ig {
		t.Error("now ignored")
	}
	if p2, _ := EnsureExcluded(repo); p2 != "" {
		t.Error("second call must do nothing")
	}
}

func TestEnsureExcludedWorktreeAndNonRepo(t *testing.T) {
	if got, err := EnsureExcluded(realTemp(t)); got != "" || err != nil {
		t.Errorf("non-repo: %q %v", got, err)
	}
	main := realTemp(t)
	gitInit(t, main)
	cmds := [][]string{
		{"-C", main, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "x"},
		{"-C", main, "worktree", "add", "-q", filepath.Join(main, "..", filepath.Base(main)+"-wt")},
	}
	for _, c := range cmds {
		if out, err := exec.Command("git", c...).CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", c, err, out)
		}
	}
	wt := filepath.Join(filepath.Dir(main), filepath.Base(main)+"-wt")
	t.Cleanup(func() { os.RemoveAll(wt) })
	wt, _ = filepath.EvalSymlinks(wt)
	p, err := EnsureExcluded(wt)
	if err != nil || p == "" {
		t.Fatalf("worktree: %q %v", p, err)
	}
	if st, err := os.Stat(filepath.Join(wt, ".git")); err != nil || st.IsDir() {
		t.Fatal("expected a .git file in the worktree")
	}
	if ig, _ := GitIgnored(wt); !ig {
		t.Error("worktree: .Wammy/ should now be ignored")
	}
}

func TestFindIndexedNonGit(t *testing.T) {
	base := realTemp(t)
	sub := filepath.Join(base, "a", "b")
	os.MkdirAll(sub, 0o755)
	if got := FindIndexed(sub); got != sub {
		t.Errorf("no index anywhere: %s", got)
	}
	os.MkdirAll(filepath.Join(base, "a", DirName), 0o700)
	os.WriteFile(filepath.Join(base, "a", DirName, "code.sqlite"), nil, 0o600)
	if got := FindIndexed(sub); got != filepath.Join(base, "a") {
		t.Errorf("FindIndexed = %s", got)
	}
}
