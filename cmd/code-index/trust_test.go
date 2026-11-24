package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// An extractor defined by <repo>/.Wammy/config.json must not run (neither its
// version_argv nor its argv) until the central file trusts the repo.
func TestUntrustedRepoExtractorIsNotExecuted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	os.MkdirAll(filepath.Join(root, ".Wammy"), 0o700)
	os.WriteFile(filepath.Join(root, "a.gb"), []byte("one\n"), 0o644)
	realRoot, _ := filepath.EvalSymlinks(root) // the central key stays the symlinked spelling
	marker := filepath.Join(tmp, "marker")
	script := filepath.Join(tmp, "ext.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >> "+marker+"\n"), 0o755)
	repoCfg, _ := json.Marshal(map[string]any{
		"languages": map[string]any{"gb": map[string]any{"extensions": []string{".gb"}, "extractor": "ext"}},
		"extractors": map[string]any{"ext": map[string]any{"type": "exec", "symbols": true,
			"argv": []string{script, "argv", "{listFile}"}, "version_argv": []string{script, "version"}}},
	})
	os.WriteFile(filepath.Join(root, ".Wammy", "config.json"), repoCfg, 0o644)
	central := filepath.Join(tmp, "config.json")
	writeCentral := func(trusted bool) {
		b, _ := json.Marshal(map[string]any{"repos": map[string]any{root: map[string]any{"trust_repo_config": trusted}}})
		os.WriteFile(central, b, 0o644)
	}
	index := func() (int, string) {
		cmd := exec.Command(os.Args[0], "index", root)
		cmd.Env = append(os.Environ(), "CODE_INDEX_TEST_CLI=1", "CODE_INDEX_CONFIG="+central, "CODE_INDEX_DB=",
			"HOME="+tmp, "USERPROFILE="+tmp, "XDG_CACHE_HOME="+filepath.Join(tmp, "cache"), "LocalAppData="+filepath.Join(tmp, "localappdata"))
		var errb bytes.Buffer
		cmd.Stderr = &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, errb.String()
	}

	writeCentral(false)
	code, errw := index()
	if _, err := os.Stat(marker); err == nil {
		b, _ := os.ReadFile(marker)
		t.Fatalf("an untrusted repo's extractor ran:\n%s", b)
	}
	if code == 0 || !strings.Contains(errw, "ignored extractors.ext") || !strings.Contains(errw, `unknown extractor "ext"`) ||
		!strings.Contains(errw, `repos."`+realRoot+`".trust_repo_config`) {
		t.Errorf("untrusted index: exit %d, stderr:\n%s", code, errw)
	}

	writeCentral(true)
	code, errw = index()
	b, _ := os.ReadFile(marker)
	if code != 0 || strings.Count(string(b), "version") != 1 || strings.Count(string(b), "argv") != 1 || strings.Contains(errw, "ignored") {
		t.Errorf("trusted index: exit %d, marker %q, stderr:\n%s", code, b, errw)
	}
}
