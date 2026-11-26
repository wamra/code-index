package indexer

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wamra/code-index/internal/config"
	"github.com/wamra/code-index/internal/ignore"
)

// candidate is a file that belongs to a configured language.
type candidate struct {
	rel   string // slash-separated, relative to the root
	abs   string
	lang  string
	size  int64
	mtime int64 // unix nanoseconds
}

// walkStats counts what the walk left out.
type walkStats struct {
	TooLarge int
	Binary   int
}

// walk lists the files under root that belong to a configured language,
// honoring .gitignore files (root and nested), the index.ignore globs and the
// secret-file patterns (config.DefaultSecretIgnore).
// Symbolic links are not followed. .git and .Wammy (the index itself) are
// always skipped, whatever the ignore files say.
func walk(root string, cfg config.Config) ([]candidate, walkStats, error) {
	var out []candidate
	var st walkStats
	base := &ignore.Set{}
	base.Add(ignore.Parse("", []byte(strings.Join(cfg.Index.Ignore, "\n"))))
	// Secret-file patterns are a separate set, never changed by a .gitignore
	// negation. Both the path and its lower-case form are tested.
	secrets := &ignore.Set{}
	secrets.Add(ignore.Parse("", []byte(strings.ToLower(strings.Join(cfg.Index.SecretPatterns(), "\n")))))
	isSecret := func(rel string, dir bool) bool {
		return secrets.Ignored(rel, dir) || secrets.Ignored(strings.ToLower(rel), dir)
	}

	var visit func(dirRel string, set *ignore.Set) error
	visit = func(dirRel string, set *ignore.Set) error {
		dirAbs := filepath.Join(root, filepath.FromSlash(dirRel))
		if data, err := os.ReadFile(filepath.Join(dirAbs, ".gitignore")); err == nil {
			set = set.Clone()
			set.Add(ignore.Parse(dirRel, data))
		}
		entries, err := os.ReadDir(dirAbs)
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			name := e.Name()
			rel := name
			if dirRel != "" {
				rel = dirRel + "/" + name
			}
			if e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if e.IsDir() {
				if name == ".git" || name == ".Wammy" || isSecret(rel, true) || set.Ignored(rel, true) {
					continue
				}
				if err := visit(rel, set); err != nil {
					return err
				}
				continue
			}
			if !e.Type().IsRegular() || isSecret(rel, false) || set.Ignored(rel, false) {
				continue
			}
			lang, ok := cfg.LanguageFor(path.Ext(name))
			if !ok {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.Size() > cfg.Index.MaxFileSize {
				st.TooLarge++
				continue
			}
			out = append(out, candidate{rel: rel, abs: filepath.Join(dirAbs, name), lang: lang, size: info.Size(), mtime: info.ModTime().UnixNano()})
		}
		return nil
	}
	if err := visit("", base); err != nil {
		return nil, st, err
	}
	return out, st, nil
}

// looksBinary reports a NUL byte in the first 8 KiB.
func looksBinary(b []byte) bool {
	if len(b) > 8192 {
		b = b[:8192]
	}
	return bytes.IndexByte(b, 0) >= 0
}
