package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CODE_INDEX_CPUPROFILE writes a CPU profile of the run.
func TestCPUProfileEnvWritesProfile(t *testing.T) {
	setup(t, "")
	prof := filepath.Join(t.TempDir(), "cpu.pprof")
	t.Setenv(EnvCPUProfile, prof)
	var out, errw bytes.Buffer
	if code := runProfiled([]string{"index", "--quiet"}, &out, &errw); code != 0 {
		t.Fatalf("index: %d %s", code, errw.String())
	}
	b, err := os.ReadFile(prof)
	if err != nil || len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b { // a gzip stream, as pprof writes
		t.Fatalf("profile: %v, %d bytes", err, len(b))
	}
	if !strings.Contains(out.String(), "indexed 5") {
		t.Errorf("the command must run as usual:\n%s", out.String())
	}

	// an unwritable path is reported, and the command still runs
	t.Setenv(EnvCPUProfile, filepath.Join(t.TempDir(), "no", "such", "dir", "cpu.pprof"))
	out.Reset()
	errw.Reset()
	if code := runProfiled([]string{"stats"}, &out, &errw); code != 0 || !strings.Contains(errw.String(), EnvCPUProfile) {
		t.Errorf("unwritable profile path: %d %q", code, errw.String())
	}
}
