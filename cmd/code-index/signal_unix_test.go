//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/proctest"
	"github.com/wamra/code-index/internal/store"
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
			if sig == syscall.SIGHUP && signal.Ignored(syscall.SIGHUP) {
				t.Skip("SIGHUP is ignored in this process (nohup); the child inherits that")
			}
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
			st, err := store.Open(filepath.Join(root, ".Wammy", "code.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var okFiles int
			if err := st.DB().QueryRow(`SELECT count(*) FROM files WHERE status='ok'`).Scan(&okFiles); err != nil {
				t.Fatal(err)
			}
			if okFiles != 0 {
				t.Errorf("the interrupted file is stored ok (%d row(s))", okFiles)
			}
		})
	}
}

// Under nohup SIGHUP arrives ignored and must stay ignored: a detached index
// run survives its terminal closing. SIGTERM still stops it.
func TestIndexKeepsAnIgnoredSIGHUPIgnored(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.gb"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extPID := filepath.Join(tmp, "ext.pid")
	script := filepath.Join(tmp, "ext.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ > "+extPID+"\nexec sleep 309\n"), 0o755); err != nil {
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
	// the shell ignores SIGHUP and execs the CLI, which inherits the ignore, as under nohup
	cmd := exec.Command(sh, "-c", `trap '' HUP; exec "$0" "$@"`, os.Args[0], "index", root)
	cmd.Env = append(os.Environ(), "CODE_INDEX_TEST_CLI=1", "CODE_INDEX_CONFIG="+cfgPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ext := readPID(t, extPID, 15*time.Second)
	defer func() {
		if proctest.Alive(ext) {
			proctest.Kill(ext)
		}
		cmd.Process.Kill()
	}()
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("index exited on an ignored SIGHUP: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if !proctest.Alive(ext) {
		t.Fatal("the extractor was killed by an ignored SIGHUP")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("index did not exit on SIGTERM")
	}
	if !proctest.WaitGone(ext, 5*time.Second) {
		t.Error("the extractor survived SIGTERM")
	}
}
