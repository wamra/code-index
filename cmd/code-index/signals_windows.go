//go:build windows

package main

import "os"

// stopSignals end a run cleanly; Windows only has the interrupt.
var stopSignals = []os.Signal{os.Interrupt}
