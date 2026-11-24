//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/proctest"
)

func readPID(t *testing.T, path string, wait time.Duration) int {
	t.Helper()
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
	}
	t.Fatalf("%s was not written", path)
	return 0
}

// SIGTERM (terminal close, CI cancel, `timeout`) stops an index run like ^C:
// the extractor lives in its own process group, so the run must cancel its
// context and kill that group, or the extractor and its children are orphaned
// until timeout_s. The files stay failed (retryable) and the lock is released.
func TestIndexSIGTERMKillsExtractorGroup(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			tmp := t.TempDir()
			root := filepath.Join(tmp, "repo")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "a.gb"), []byte("one\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			extPID, gcPID := filepath.Join(tmp, "ext.pid"), filepath.Join(tmp, "gc.pid")
			script := filepath.Join(tmp, "ext.sh")
			body := "#!/bin/sh\necho $$ > " + extPID + "\nsleep 307 &\necho $! > " + gcPID + "\nwait\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{
				"languages":  map[string]any{"gb": map[string]any{"extensions": []string{".gb"}, "extractor": "ext"}},
				"extractors": map[string]any{"ext": map[string]any{"type": "exec", "argv": []string{script, "{listFile}"}, "symbols": true, "timeout_s": 600}},
			}
			b, _ := json.Marshal(cfg)
			cfgPath := filepath.Join(tmp, "config.json")
			if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "index", root)
			cmd.Env = append(os.Environ(), "CODE_INDEX_TEST_CLI=1", "CODE_INDEX_CONFIG="+cfgPath)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var ext, gc int
			defer func() {
				for _, p := range []int{ext, gc} {
					if p > 0 && proctest.Alive(p) {
						proctest.Kill(p)
					}
				}
				cmd.Process.Kill()
			}()
			ext = readPID(t, extPID, 15*time.Second)
			gc = readPID(t, gcPID, 15*time.Second)
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				ee, ok := err.(*exec.ExitError)
				if err == nil || !ok || ee.ExitCode() <= 0 {
					t.Errorf("index must exit with a non-zero code after %v, got %v", sig, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("index did not exit")
			}
			for name, p := range map[string]int{"extractor": ext, "grandchild": gc} {
				if !proctest.WaitGone(p, 5*time.Second) {
					t.Errorf("%s %d survived %v of the index run", name, p, sig)
				}
			}
			// the lock is released and the file stays retryable
			if _, err := os.Stat(filepath.Join(root, ".Wammy", "code.sqlite")); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("sqlite3", filepath.Join(root, ".Wammy", "code.sqlite"), "select status from files").CombinedOutput()
			if err == nil && strings.TrimSpace(string(out)) == "ok" {
				t.Errorf("the interrupted file is stored ok: %s", out)
			}
		})
	}
}
