//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// stopSignals end a run cleanly: ^C, and the signals a terminal close
// (SIGHUP), `timeout`, a service manager or a CI cancel (SIGTERM) send. An
// exec extractor runs in its own process group, so it would not see them; the
// run cancels its context and kills that group instead. SIGHUP is left alone
// when it was inherited as ignored (nohup, disown): asking to be notified
// would switch it back on and a detached run would die with its terminal.
func stopSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}
