//go:build !windows

package main

import (
	"os"
	"syscall"
)

func processSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
}

func isReloadSignal(signal os.Signal) bool {
	return signal == syscall.SIGHUP
}
