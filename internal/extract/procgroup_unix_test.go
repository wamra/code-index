//go:build !windows

package extract

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wamra/code-index/internal/config"
)

func waitGone(t *testing.T, pidfile string) {
	t.Helper()
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the helper did not record its grandchild: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	alive := func() bool { return syscall.Kill(pid, 0) == nil }
	defer func() {
		if alive() {
			syscall.Kill(pid, syscall.SIGKILL) // do not leak it from a failing test
		}
	}()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !alive() {
			return
		}
	}
	t.Errorf("grandchild %d is still running after the extractor timed out", pid)
}

// A timed-out exec extractor takes its whole process group with it: no
// grandchild survives, and the call does not wait out the pipe-drain delay.
func TestExecTimeoutKillsGrandchildren(t *testing.T) {
	root := t.TempDir()
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	ec := helperEC("spawn")
	ec.TimeoutS = 2
	ec.Env["EXTRACT_TEST_PIDFILE"] = pidfile
	start := time.Now()
	_, _, err := runEdges(t, ec, Env{Root: root}, []string{filepath.Join(root, "a.x")})
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out after 2s") {
		t.Fatalf("err = %v", err)
	}
	if took > 2*time.Second+3*time.Second { // timeout + well under the old 5 s WaitDelay
		t.Errorf("the call took %v, want about the 2 s timeout", took)
	}
	waitGone(t, pidfile)
}

// The version command is bounded the same way (and a grandchild holding its
// stdout no longer blocks the call).
func TestVersionTimeoutKillsGrandchildren(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "grandchild.pid")
	ec := config.Extractor{
		VersionArgv: []string{os.Args[0], "-test.run=^$"},
		Env:         map[string]string{"EXTRACT_TEST_HELPER": "spawn", "EXTRACT_TEST_PIDFILE": pidfile},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if v := ConfiguredVersion(ctx, ec); v != "" {
		t.Errorf("version = %q", v)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("the call took %v, want about 1.5 s", took)
	}
	waitGone(t, pidfile)
}
