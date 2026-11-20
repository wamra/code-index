package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseASTLinesFromRecordedSample(t *testing.T) {
	f, err := os.Open("testdata/powershell-ast.sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, skipped, err := parseASTLines(f)
	if err != nil || skipped != 0 || len(recs) != 18 {
		t.Fatalf("recs=%d skipped=%d err=%v", len(recs), skipped, err)
	}
	by := map[string][]astRec{}
	for _, r := range recs {
		by[r.T] = append(by[r.T], r)
	}
	if a := by["alias"]; len(a) != 1 || a[0].Alias != "gw" || a[0].Target != "Get-Widget" || a[0].Line != 27 || a[0].Col != 17 {
		t.Errorf("alias = %+v", a)
	}
	if d := by["dynamic"]; len(d) != 1 || d[0].Expr != "& $cmd" || d[0].File != "main.ps1" || d[0].Line != 9 {
		t.Errorf("dynamic = %+v", d)
	}
	if d := by["dotsource"]; len(d) != 1 || d[0].Path != "lib.ps1" {
		t.Errorf("dotsource = %+v", d)
	}
	if d := by["import"]; len(d) != 1 || d[0].Path != "Widgets/Widgets.psd1" || d[0].File != "lib.ps1" {
		t.Errorf("import = %+v", d)
	}
	var inHelper bool
	for _, c := range by["call"] {
		if c.Name == "Get-Widget" && c.In == "Invoke-Helper" && c.File == "lib.ps1" && c.Line == 10 {
			inHelper = true
		}
	}
	if !inHelper || len(by["func"]) != 4 || len(by["parse_error"]) != 1 || by["parse_error"][0].Count != 3 {
		t.Errorf("calls=%+v funcs=%d parse_errors=%+v", by["call"], len(by["func"]), by["parse_error"])
	}
}

func TestParseASTLinesSkipsNoise(t *testing.T) {
	in := "WARNING: something\n\n{\"t\":\"call\",\"name\":\"X\",\"file\":\"a.ps1\",\"line\":1,\"col\":1}\n{broken\n{\"nope\":1}\n"
	recs, skipped, err := parseASTLines(strings.NewReader(in))
	if err != nil || len(recs) != 1 || skipped != 3 {
		t.Fatalf("recs=%+v skipped=%d err=%v", recs, skipped, err)
	}
}

// The real extractor script, run with the pwsh on this machine. Skipped when
// pwsh is not installed.
func TestRealPowerShellExtractor(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not installed")
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("LocalAppData", filepath.Join(tmp, "localappdata"))
	src := `function Get-Thing {
    [Alias('gt')]
    param([Alias('n')][string]$Name)
    iex "Write-Output $Name"
}
Set-Alias -Name gt2 -Value Get-Thing
sal gt3 Get-Thing
. .\other.ps1
Import-Module Things -Force
& $handler 1
gt2 -Name x
`
	file := filepath.Join(tmp, "t.ps1")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := defaultExtract(context.Background(), ExtractorPowerShellAST, []string{file})
	if err != nil {
		t.Fatal(err)
	}
	recs, skipped, err := parseASTLines(strings.NewReader(string(out)))
	if err != nil || skipped != 0 {
		t.Fatalf("skipped=%d err=%v\n%s", skipped, err, out)
	}
	have := map[string]bool{}
	for _, r := range recs {
		switch r.T {
		case "alias":
			have["alias:"+r.Alias+">"+r.Target] = true
		case "dynamic":
			have["dynamic:"+r.Expr] = true
		case "dotsource", "import":
			have[r.T+":"+r.Path] = true
		case "call":
			have["call:"+r.Name+"@"+r.In] = true
		}
	}
	for _, want := range []string{
		"alias:gt>Get-Thing", "alias:gt2>Get-Thing", "alias:gt3>Get-Thing", // [Alias()], Set-Alias, positional sal
		"dynamic:& $handler", "dynamic:iex \"Write-Output $Name\"",
		"dotsource:.\\other.ps1", "import:Things", "call:gt2@",
	} {
		if !have[want] {
			t.Errorf("missing %q in %v", want, have)
		}
	}
	if have["alias:n>Get-Thing"] {
		t.Error("a parameter [Alias()] is not a command alias")
	}
}
