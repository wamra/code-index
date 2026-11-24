package indexer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/wamra/code-index/internal/extract"
)

func TestParseASTLinesFromRecordedSample(t *testing.T) {
	f, err := os.Open("testdata/powershell-ast.sample.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, skipped, err := extract.ParseLines(f)
	if err != nil || skipped != 0 || len(recs) != 18 {
		t.Fatalf("recs=%d skipped=%d err=%v", len(recs), skipped, err)
	}
	by := map[string][]extract.Record{}
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
	recs, skipped, err := extract.ParseLines(strings.NewReader(in))
	if err != nil || len(recs) != 1 || skipped != 3 {
		t.Fatalf("recs=%+v skipped=%d err=%v", recs, skipped, err)
	}
}

// The real extractor script, run once per PowerShell on this machine: pwsh
// (7) and powershell (Windows PowerShell 5.1). Skipped when neither is installed.
func TestRealPowerShellExtractor(t *testing.T) {
	ran := false
	for _, interp := range []string{"pwsh", "powershell"} {
		if _, err := exec.LookPath(interp); err != nil {
			continue
		}
		ran = true
		t.Run(interp, func(t *testing.T) { realPowerShellExtractor(t, interp) })
	}
	if !ran {
		t.Skip("no PowerShell installed")
	}
}

func realPowerShellExtractor(t *testing.T, interp string) {
	t.Setenv(PowerShellEnv, interp)
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
Write-Output "h\u00e9llo \u00fcber"; Get-Accent
`
	// Raw strings do not decode escapes; keep this file ASCII and decode here.
	src = strings.NewReplacer(`\u00e9`, "\u00e9", `\u00fc`, "\u00fc").Replace(src)
	file := filepath.Join(tmp, "t.ps1")
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := defaultExtract(context.Background(), ExtractorPowerShellAST, []string{file})
	if err != nil {
		t.Fatal(err)
	}
	recs, skipped, err := extract.ParseLines(strings.NewReader(string(out)))
	if err != nil || skipped != 0 {
		t.Fatalf("skipped=%d err=%v\n%s", skipped, err, out)
	}
	// Columns are 1-based UTF-16 units, so a non-ASCII character before a call
	// must not shift it (5.1 would, if it decoded the file as ANSI).
	accentLine, accentCol := 0, 0
	for i, l := range strings.Split(src, "\n") {
		if j := strings.Index(l, "Get-Accent"); j >= 0 {
			accentLine, accentCol = i+1, len(utf16.Encode([]rune(l[:j])))+1
		}
	}
	var gotAccent bool
	for _, r := range recs {
		if r.T == "call" && r.Name == "Get-Accent" {
			gotAccent = true
			if r.Line != accentLine || r.Col != accentCol {
				t.Errorf("Get-Accent at %d:%d, want %d:%d", r.Line, r.Col, accentLine, accentCol)
			}
		}
	}
	if !gotAccent {
		t.Errorf("no call record for Get-Accent in %s", out)
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

func TestFindPowerShell(t *testing.T) {
	onPath := func(names ...string) func(string) (string, error) {
		return func(n string) (string, error) {
			for _, have := range names {
				if n == have {
					return "/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	for _, c := range []struct {
		name, override string
		path           []string
		want           string // empty: ErrExtractorUnavailable
	}{
		{"pwsh preferred", "", []string{"pwsh", "powershell"}, "/bin/pwsh"},
		{"fallback to powershell", "", []string{"powershell"}, "/bin/powershell"},
		{"neither", "", nil, ""},
		{"override wins", "powershell", []string{"pwsh", "powershell"}, "/bin/powershell"},
		{"override path", "/opt/ps/pwsh", []string{"/opt/ps/pwsh"}, "/bin//opt/ps/pwsh"},
		{"override missing does not fall back", "nope", []string{"pwsh"}, ""},
	} {
		got, err := findPowerShell(c.override, onPath(c.path...))
		if c.want == "" {
			if !errors.Is(err, ErrExtractorUnavailable) || got != "" {
				t.Errorf("%s: got %q, %v", c.name, got, err)
			}
		} else if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v want %q", c.name, got, err, c.want)
		}
	}
}

// Windows PowerShell 5.1 reads a BOM-less script as ANSI, so a non-ASCII byte
// in a shipped script can break parsing there.
func TestExtractorScriptsAreASCII(t *testing.T) {
	files, err := filepath.Glob("../../extractors/*.ps1")
	if err != nil || len(files) == 0 {
		t.Fatalf("no extractor scripts found: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, b := range data {
			if b >= 0x80 {
				line := 1 + strings.Count(string(data[:i]), "\n")
				t.Errorf("%s:%d: non-ASCII byte 0x%02x", filepath.Base(f), line, b)
				break
			}
		}
	}
}
