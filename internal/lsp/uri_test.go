package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathToURIAndBack(t *testing.T) {
	cases := []struct {
		path, uri string
		windows   bool
	}{
		{`C:\Users\dev\My Project\a.ps1`, "file:///C:/Users/dev/My%20Project/a.ps1", true},
		{`c:\x\y.sh`, "file:///c:/x/y.sh", true},
		{`C:/already/slashes.ps1`, "file:///C:/already/slashes.ps1", true},
		{`\\srv\share\dir\f.ps1`, "file://srv/share/dir/f.ps1", true},
		{`C:\50% #1\a.ps1`, "file:///C:/50%25%20%231/a.ps1", true},
		{`/home/u/proj/a.sh`, "file:///home/u/proj/a.sh", false},
		{`/home/u/a b/ü.sh`, "file:///home/u/a%20b/%C3%BC.sh", false},
	}
	for _, c := range cases {
		if got := pathToURI(c.path, c.windows); got != c.uri {
			t.Errorf("pathToURI(%q) = %q, want %q", c.path, got, c.uri)
		}
		back, err := uriToPath(c.uri, c.windows)
		if err != nil {
			t.Fatal(err)
		}
		if c.windows {
			wantWin := map[string]string{
				"file:///C:/Users/dev/My%20Project/a.ps1": `C:\Users\dev\My Project\a.ps1`,
				"file:///c:/x/y.sh":                       `c:\x\y.sh`,
				"file:///C:/already/slashes.ps1":          `C:\already\slashes.ps1`,
				"file://srv/share/dir/f.ps1":              `\\srv\share\dir\f.ps1`,
				"file:///C:/50%25%20%231/a.ps1":           `C:\50% #1\a.ps1`,
			}[c.uri]
			if back != wantWin {
				t.Errorf("uriToPath(%q) = %q, want %q", c.uri, back, wantWin)
			}
		} else if back != c.path {
			t.Errorf("uriToPath(%q) = %q, want %q", c.uri, back, c.path)
		}
	}
	if _, err := uriToPath("https://example.com/x", false); err == nil {
		t.Error("non-file URI must fail")
	}
}

func TestSameFileDriveLetterAndEncoding(t *testing.T) {
	yes := [][2]string{
		{"file:///c%3A/Users/x/a.ps1", "file:///C:/Users/x/a.ps1"},
		{"file:///C:/Users/x/a.ps1", "file:///c:/users/x/A.ps1"},
		{"file:///C:/a%20b.ps1", "file:///C:/a b.ps1"},
	}
	for _, p := range yes {
		if !sameFile(p[0], p[1], true) {
			t.Errorf("windows: %q and %q should be the same file", p[0], p[1])
		}
	}
	if sameFile("file:///C:/a.ps1", "file:///D:/a.ps1", true) {
		t.Error("different drives")
	}
	if sameFile("file:///tmp/A.sh", "file:///tmp/a.sh", false) {
		t.Error("unix paths are case-sensitive")
	}
}

func TestPathToURIRoundTripOnThisOS(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dir with space", "f.sh")
	back, err := URIToPath(PathToURI(p))
	if err != nil || back != p {
		t.Errorf("round trip %q -> %q (%v)", p, back, err)
	}
}

func TestResolveCommandUsesPathAndPathExt(t *testing.T) {
	dir := t.TempDir()
	name, file := "fake-server", "fake-server"
	if runtime.GOOS == "windows" {
		file = "fake-server.cmd" // npm-style shim, found through PATHEXT
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := ResolveCommand(name)
	if err != nil || filepath.Dir(got) != dir {
		t.Fatalf("ResolveCommand = %q, %v", got, err)
	}
	if _, err := ResolveCommand("no-such-server-anywhere"); err == nil {
		t.Error("missing command must fail")
	}
}
