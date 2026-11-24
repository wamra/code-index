//go:build windows

package extract

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// cleanupDaemon: Windows does not kill the process group, so just stop the leftover.
func cleanupDaemon(t *testing.T, pidfile string) {
	b, err := os.ReadFile(pidfile)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	}
}
