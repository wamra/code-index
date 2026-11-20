package indexer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wamra/code-index/extractors"
)

// ExtractorPowerShellAST is the config value of a language's "extractor" that
// selects the PowerShell AST extractor.
const ExtractorPowerShellAST = "powershell-ast"

// ErrExtractorUnavailable means the interpreter an extractor needs is missing.
var ErrExtractorUnavailable = errors.New("extractor interpreter not found")

// ExtractFunc runs the named extractor over absolute file paths and returns
// its JSON-lines output.
type ExtractFunc func(ctx context.Context, name string, files []string) ([]byte, error)

// astRec is one JSON line of the PowerShell extractor. Line and Col are
// 1-based, as the PowerShell parser reports them.
type astRec struct {
	T       string `json:"t"` // func | alias | call | dynamic | dotsource | import | parse_error | error
	File    string `json:"file"`
	Name    string `json:"name"`
	Alias   string `json:"alias"`
	Target  string `json:"target"`
	Path    string `json:"path"`
	Expr    string `json:"expr"`
	In      string `json:"in"`
	Message string `json:"message"`
	First   string `json:"first"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	EndLine int    `json:"endLine"`
	EndCol  int    `json:"endCol"`
	Count   int    `json:"count"`
}

// parseASTLines decodes the extractor output. Lines that are not JSON objects
// (a warning pwsh printed to stdout, say) are counted and skipped.
func parseASTLines(r io.Reader) (recs []astRec, skipped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec astRec
		if line[0] != '{' || json.Unmarshal(line, &rec) != nil || rec.T == "" {
			skipped++
			continue
		}
		recs = append(recs, rec)
	}
	return recs, skipped, sc.Err()
}

// defaultExtract runs an extractor script embedded in the binary. The script
// is written to <user cache dir>/code-index/extractors/ (named by its hash, so
// an upgrade never runs a stale copy) and executed once for all files.
func defaultExtract(ctx context.Context, name string, files []string) ([]byte, error) {
	if name != ExtractorPowerShellAST {
		return nil, fmt.Errorf("unknown extractor %q", name)
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		return nil, fmt.Errorf("%w: pwsh is not on PATH", ErrExtractorUnavailable)
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "code-index", "extractors")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(extractors.PowerShellAST)
	script := filepath.Join(dir, "powershell-ast-"+hex.EncodeToString(sum[:4])+".ps1")
	if cur, err := os.ReadFile(script); err != nil || !bytes.Equal(cur, extractors.PowerShellAST) {
		if err := os.WriteFile(script, extractors.PowerShellAST, 0o644); err != nil {
			return nil, err
		}
	}
	list, err := os.CreateTemp("", "code-index-files-*.txt")
	if err != nil {
		return nil, err
	}
	defer os.Remove(list.Name())
	if _, err := list.WriteString(strings.Join(files, "\n") + "\n"); err != nil {
		list.Close()
		return nil, err
	}
	list.Close()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	args := []string{"-NoLogo", "-NoProfile", "-NonInteractive"}
	if runtime.GOOS == "windows" {
		args = append(args, "-ExecutionPolicy", "Bypass") // the script is ours, written to the user cache
	}
	cmd := exec.CommandContext(ctx, pwsh, append(args, "-File", script, "-ListFile", list.Name())...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("pwsh extractor: %w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}
