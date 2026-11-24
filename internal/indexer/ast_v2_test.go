package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wamra/code-index/internal/extract"
)

// Every call record of the PowerShell extractor carries resolution "exact"
// (schema v2: the extractor gives a name only; code-index derives alias itself).
func TestPowerShellCallsCarryResolution(t *testing.T) {
	f, err := os.Open("testdata/powershell-ast.sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, _, err := extract.ParseLines(f)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, r := range recs {
		if r.T == extract.TypeCall {
			calls++
			if r.Resolution != extract.ResExact {
				t.Errorf("sample call %+v lacks resolution exact", r)
			}
		}
	}
	if calls == 0 {
		t.Fatal("no call records in the sample")
	}

	interp := ""
	for _, name := range []string{"pwsh", "powershell"} {
		if _, err := exec.LookPath(name); err == nil {
			interp = name
			break
		}
	}
	if interp == "" {
		t.Skip("no PowerShell installed (the recorded sample was checked)")
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("LocalAppData", filepath.Join(tmp, "localappdata"))
	file := filepath.Join(tmp, "t.ps1")
	if err := os.WriteFile(file, []byte("function A {\n  B\n}\nA\nWrite-Output x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	live, notes, err := runPowerShellAST(t, interp, []string{file})
	if err != nil || len(notes) != 0 {
		t.Fatalf("notes=%v err=%v", notes, err)
	}
	n := 0
	for _, r := range live {
		if r.T == extract.TypeCall {
			n++
			if r.Resolution != extract.ResExact {
				t.Errorf("%s: call %q has resolution %q", interp, r.Name, r.Resolution)
			}
		}
	}
	if n != 3 {
		t.Errorf("%s: %d call records, want 3", interp, n)
	}
}
