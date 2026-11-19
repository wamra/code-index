// Package workspace maps a directory to its repo root and index database.
//
// Every repo has its own index at <root>/.Wammy/code.sqlite. The root is the
// nearest ancestor that contains .git (a directory, or a file in worktrees and
// submodules); outside a git repo it is the directory itself.
package workspace

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DirName is the per-repo directory that holds the index.
const DirName = ".Wammy"

// Root returns the repo root for dir (symlinks resolved).
func Root(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return abs
		}
	}
}

// DBPath is the database for root: $CODE_INDEX_DB, else <root>/.Wammy/code.sqlite.
func DBPath(root string, getenv func(string) string) string {
	if p := getenv("CODE_INDEX_DB"); p != "" {
		return p
	}
	return filepath.Join(root, DirName, "code.sqlite")
}

func isGitRepo(root string) bool {
	_, err := os.Lstat(filepath.Join(root, ".git"))
	return err == nil
}

func gitOnPath() bool { _, err := exec.LookPath("git"); return err == nil }

// GitIgnored reports whether .Wammy/ is ignored in the git repo at root.
// known is false when root is not a git repo, git is missing, or git failed.
func GitIgnored(root string) (ignored, known bool) {
	if !isGitRepo(root) || !gitOnPath() {
		return false, false
	}
	err := exec.Command("git", "-C", root, "check-ignore", "-q", DirName+"/").Run()
	if err == nil {
		return true, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// EnsureExcluded appends ".Wammy/" to the repo's info/exclude when root is a
// git repo and the directory is not already ignored. Tracked files such as
// .gitignore are never touched. It returns the file written to ("" when
// nothing was needed or git is unavailable).
func EnsureExcluded(root string) (string, error) {
	ignored, known := GitIgnored(root)
	if !known || ignored {
		return "", nil
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return "", nil
	}
	p := strings.TrimSpace(string(bytes.TrimSpace(out)))
	if p == "" {
		return "", nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	old, _ := os.ReadFile(p)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	line := DirName + "/\n"
	if len(old) > 0 && old[len(old)-1] != '\n' {
		line = "\n" + line
	}
	if _, err := f.WriteString(line); err != nil {
		return "", err
	}
	return p, nil
}
