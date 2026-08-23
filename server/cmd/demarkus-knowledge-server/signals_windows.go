//go:build windows

package main

import "os"

func processSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func isReloadSignal(os.Signal) bool {
	return false
}
