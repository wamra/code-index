//go:build !windows

package main

import (
	"os"
	"syscall"
)

// stopSignals end a run cleanly: ^C, and the signals a terminal close
// (SIGHUP), `timeout`, a service manager or a CI cancel (SIGTERM) send. An
// exec extractor runs in its own process group, so it would not see them; the
// run cancels its context and kills that group instead.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
