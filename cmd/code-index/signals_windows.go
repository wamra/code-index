//go:build windows

package main

import "os"

// stopSignals end a run cleanly; Windows only has the interrupt.
func stopSignals() []os.Signal { return []os.Signal{os.Interrupt} }
