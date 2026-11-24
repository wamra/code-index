package extract

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/wamra/code-index/internal/config"
)

// VersionTimeout bounds a version_argv command.
const VersionTimeout = 10 * time.Second

// ConfiguredVersion is the cache-key version of an extractor entry that the
// config states: ec.Version when set, else the first line of the trimmed
// stdout of ec.VersionArgv (run with ec.Env, no placeholders, 10 s timeout),
// else "". A version command that is missing, fails or times out gives "" (the
// version is unknown, which the fingerprint tolerates). It never starts a
// language server. Both extractor types call it from their Version method.
func ConfiguredVersion(ctx context.Context, ec config.Extractor) string {
	if ec.Version != "" {
		return ec.Version
	}
	if len(ec.VersionArgv) == 0 {
		return ""
	}
	path, err := exec.LookPath(ec.VersionArgv[0])
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, VersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, ec.VersionArgv[1:]...)
	killOnCancel(cmd)
	cmd.WaitDelay = execWaitDelay
	cmd.Env = os.Environ()
	keys := make([]string, 0, len(ec.Env))
	for k := range ec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+ec.Env[k])
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(stdout.String()), "\n")
	return strings.TrimSpace(first)
}
