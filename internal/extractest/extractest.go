// Package extractest serves recorded extractor output to tests: the golden
// output of `wammybash extract` with its root rewritten, as a function and as
// a subprocess (the test binary re-executed), so that tests exercise exec
// extractors without the real tool.
package extractest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvServe makes Main serve instead of running the tests; EnvGolden names the
// golden file it serves.
const (
	EnvServe  = "CODE_INDEX_FAKE_EXTRACT_SERVE"
	EnvGolden = "CODE_INDEX_FAKE_EXTRACT_GOLDEN"
)

const rootMark = "<root>/"

// Rewrite returns the golden JSON lines with "<root>/" in the file, path and
// to keys replaced by the absolute root (native separators). When files is not
// nil only the records of those absolute paths are kept.
func Rewrite(golden []byte, root string, files []string) ([]byte, error) {
	want := map[string]bool{}
	for _, f := range files {
		want[f] = true
	}
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(golden))
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("golden line %q: %w", line, err)
		}
		for _, k := range []string{"file", "path", "to"} {
			if s, ok := m[k].(string); ok && strings.HasPrefix(s, rootMark) {
				m[k] = filepath.Join(root, filepath.FromSlash(s[len(rootMark):]))
			}
		}
		if files != nil && !want[m["file"].(string)] {
			continue
		}
		enc, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		out.Write(enc)
		out.WriteByte('\n')
	}
	return out.Bytes(), sc.Err()
}

// Main serves the golden output when EnvServe is "1", as `wammybash extract
// --root DIR --list-file FILE` would, and returns true; it returns false when
// the process is a normal test run. Call it first in TestMain.
func Main() bool {
	if os.Getenv(EnvServe) != "1" {
		return false
	}
	var root, list string
	for i, a := range os.Args {
		switch a {
		case "--root":
			root = os.Args[i+1]
		case "--list-file":
			list = os.Args[i+1]
		}
	}
	fail := func(err error) bool {
		fmt.Fprintln(os.Stderr, "fake extractor:", err)
		os.Exit(1)
		return true
	}
	golden, err := os.ReadFile(os.Getenv(EnvGolden))
	if err != nil {
		return fail(err)
	}
	var files []string
	if list != "" {
		b, err := os.ReadFile(list)
		if err != nil {
			return fail(err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				files = append(files, l)
			}
		}
	}
	out, err := Rewrite(golden, root, files)
	if err != nil {
		return fail(err)
	}
	os.Stdout.Write(out)
	return true
}
