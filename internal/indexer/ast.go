package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wamra/code-index/extractors"
	"github.com/wamra/code-index/internal/extract"
)

// ExtractorPowerShellAST is the config value of a language's "extractor" that
// selects the PowerShell AST extractor.
const ExtractorPowerShellAST = "powershell-ast"

// ErrExtractorUnavailable means the interpreter an extractor needs is missing.
// It is extract.ErrExtractorUnavailable, so errors.Is matches either name.
var ErrExtractorUnavailable = extract.ErrExtractorUnavailable

// ExtractFunc runs the named extractor over absolute file paths and returns
// its JSON-lines output.
type ExtractFunc = extract.ExecFunc

// PowerShellEnv names an environment variable that overrides the PowerShell
// interpreter (a path or a name on PATH).
const PowerShellEnv = "CODE_INDEX_POWERSHELL"

// findPowerShell picks the interpreter: the override when set, else pwsh
// (PowerShell 7), else powershell (Windows PowerShell 5.1).
func findPowerShell(override string, lookPath func(string) (string, error)) (string, error) {
	if override != "" {
		p, err := lookPath(override)
		if err != nil {
			return "", fmt.Errorf("%w: %s=%q was not found", ErrExtractorUnavailable, PowerShellEnv, override)
		}
		return p, nil
	}
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := lookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: neither pwsh nor powershell is on PATH", ErrExtractorUnavailable)
}

// defaultExtract runs an extractor script embedded in the binary. The script
// is written to <user cache dir>/code-index/extractors/ (named by its hash, so
// an upgrade never runs a stale copy) and executed once for all files.
func defaultExtract(ctx context.Context, name string, files []string) ([]byte, error) {
	if name != ExtractorPowerShellAST {
		return nil, fmt.Errorf("unknown extractor %q", name)
	}
	pwsh, err := findPowerShell(os.Getenv(PowerShellEnv), exec.LookPath)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("powershell extractor: %w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}
