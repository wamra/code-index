// Package workspace maps a workspace root to its index database.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
)

// ID is the first 16 hex characters of sha256(real path of root).
func ID(root string) string {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		real = root
	}
	sum := sha256.Sum256([]byte(real))
	return hex.EncodeToString(sum[:])[:16]
}

// CacheDir is the per-user cache root: ~/Library/Caches (macOS),
// $XDG_CACHE_HOME or ~/.cache (Linux), %LOCALAPPDATA% (Windows).
func CacheDir(home string, getenv func(string) string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Caches")
	case "windows":
		if v := getenv("LOCALAPPDATA"); v != "" {
			return v
		}
		return filepath.Join(home, "AppData", "Local")
	}
	if v := getenv("XDG_CACHE_HOME"); v != "" {
		return v
	}
	return filepath.Join(home, ".cache")
}

// DBPath is the database for root: $CODE_INDEX_DB, else
// <cache>/code-index/<workspace-id>/index.sqlite.
func DBPath(root, home string, getenv func(string) string) string {
	if p := getenv("CODE_INDEX_DB"); p != "" {
		return p
	}
	return filepath.Join(CacheDir(home, getenv), "code-index", ID(root), "index.sqlite")
}

// Find picks the workspace root for a command run in dir: the nearest
// ancestor of dir (dir included) that already has an index database, else dir.
// With CODE_INDEX_DB set the root is dir (the caller reads meta.root).
func Find(dir, home string, getenv func(string) string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	if getenv("CODE_INDEX_DB") != "" {
		return abs
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(DBPath(d, home, getenv)); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return abs
		}
	}
}
