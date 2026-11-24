//go:build !windows

package proctest

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Alive reports whether pid is a running process. A zombie (killed but not yet
// reaped, as in a container whose PID 1 does not reap) is not alive: kill(0)
// still succeeds for it, so on Linux the state in /proc/<pid>/stat decides.
func Alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if runtime.GOOS == "linux" {
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			// "pid (comm) S ...": the state follows the last ')'
			s := string(b)
			if i := strings.LastIndexByte(s, ')'); i >= 0 && len(s) > i+2 && s[i+2] == 'Z' {
				return false
			}
		}
	}
	return true
}

// WaitGone waits up to d for pid to stop being alive and reports whether it did.
func WaitGone(pid int, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !Alive(pid) {
			return true
		}
	}
	return !Alive(pid)
}

// Kill sends SIGKILL to pid (cleanup of a failing test).
func Kill(pid int) { syscall.Kill(pid, syscall.SIGKILL) }
