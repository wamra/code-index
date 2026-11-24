//go:build windows

package extract

import "os/exec"

// killOnCancel is a no-op on Windows: exec.CommandContext kills the direct
// child only, and a grandchild it started keeps running (extractors/README.md).
func killOnCancel(cmd *exec.Cmd) {}
