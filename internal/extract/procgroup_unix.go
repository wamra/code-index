//go:build !windows

package extract

import (
	"os"
	"os/exec"
	"syscall"
)

// killOnCancel starts cmd in its own process group and makes the context's
// cancellation kill the whole group, so a grandchild the extractor started
// does not outlive the run or hold its stdout open.
func killOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// the child leads its group (Setpgid), so -pid is the group
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH { // the group is already gone
			return os.ErrProcessDone
		}
		return err
	}
}
