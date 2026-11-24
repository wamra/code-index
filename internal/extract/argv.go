package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wamra/code-index/extractors"
)

// Placeholders in an extractor's argv template.
const (
	PlaceholderRoot     = "{root}"     // the absolute workspace root
	PlaceholderListFile = "{listFile}" // exec only: a temp file with one absolute path per line
	embeddedPrefix      = "{embedded:" // {embedded:NAME}: path of an embedded extractor file
)

var placeholderRe = regexp.MustCompile(`\{[^{}]*\}`)

// Placeholders returns the placeholder tokens of one argv element, in order
// ("{root}", "{embedded:powershell-ast.ps1}", or any other "{...}" text, which
// is an unknown placeholder). A brace that is not part of a "{...}" pair is
// literal text.
func Placeholders(arg string) []string { return placeholderRe.FindAllString(arg, -1) }

// IsKnownPlaceholder reports whether tok (one element of Placeholders) is
// {root}, {listFile} or {embedded:NAME} with an embedded file NAME that exists.
func IsKnownPlaceholder(tok string) bool {
	switch tok {
	case PlaceholderRoot, PlaceholderListFile:
		return true
	}
	if name, ok := embeddedName(tok); ok {
		_, found := extractors.Embedded(name)
		return found
	}
	return false
}

func embeddedName(tok string) (string, bool) {
	if strings.HasPrefix(tok, embeddedPrefix) && strings.HasSuffix(tok, "}") {
		return tok[len(embeddedPrefix) : len(tok)-1], true
	}
	return "", false
}

// Vars supplies the values of the placeholders for ExpandArgv.
type Vars struct {
	Root string
	// ListFile is the list file path; empty makes {listFile} an error (an lsp
	// extractor has none).
	ListFile string
	// Embedded returns the path of {embedded:NAME}; nil uses EmbeddedPath.
	Embedded func(name string) (string, error)
}

// ExpandArgv replaces the placeholders in every element of argv. A placeholder
// may be a whole element or part of one. An unknown placeholder is an error.
func ExpandArgv(argv []string, v Vars) ([]string, error) {
	embedded := v.Embedded
	if embedded == nil {
		embedded = EmbeddedPath
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		var err error
		out[i] = placeholderRe.ReplaceAllStringFunc(a, func(tok string) string {
			switch {
			case tok == PlaceholderRoot:
				return v.Root
			case tok == PlaceholderListFile:
				if v.ListFile == "" {
					err = fmt.Errorf("%s is not available for this extractor", tok)
				}
				return v.ListFile
			}
			if name, ok := embeddedName(tok); ok {
				p, e := embedded(name)
				if e != nil {
					err = e
				}
				return p
			}
			err = fmt.Errorf("unknown placeholder %s in argv element %q", tok, a)
			return tok
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// embeddedFile returns the contents of an embedded extractor file, its
// content-hash name ("powershell-ast-1a2b3c4d.ps1") and the short hash.
func embeddedFile(name string) (data []byte, hashed, hash string, err error) {
	data, ok := extractors.Embedded(name)
	if !ok {
		return nil, "", "", fmt.Errorf("no embedded extractor file %q", name)
	}
	sum := sha256.Sum256(data)
	hash = hex.EncodeToString(sum[:4])
	ext := filepath.Ext(name)
	return data, strings.TrimSuffix(name, ext) + "-" + hash + ext, hash, nil
}

// EmbeddedPath writes the embedded extractor file called name to
// <user cache dir>/code-index/extractors/ under a content-hash name (an upgrade
// never runs a stale copy; the directory is created, an up-to-date copy is
// kept) and returns its path. It is what {embedded:NAME} expands to.
func EmbeddedPath(name string) (string, error) {
	data, hashed, _, err := embeddedFile(name)
	if err != nil {
		return "", err
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "code-index", "extractors")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, hashed)
	if cur, err := os.ReadFile(p); err != nil || string(cur) != string(data) {
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return "", err
		}
	}
	return p, nil
}

// IdentityArgv is the argv that goes into the cache fingerprint: argv with
// {root} replaced by root, {embedded:NAME} replaced by "embedded:NAME@<hash>"
// (the content hash of the embedded file, nothing is written) and {listFile}
// left as the literal text "{listFile}", so the value is the same on every run
// of the same configuration and binary.
func IdentityArgv(argv []string, root string) ([]string, error) {
	return ExpandArgv(argv, Vars{
		Root:     root,
		ListFile: PlaceholderListFile,
		Embedded: func(name string) (string, error) {
			_, _, hash, err := embeddedFile(name)
			return "embedded:" + name + "@" + hash, err
		},
	})
}
