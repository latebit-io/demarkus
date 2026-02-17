//go:build windows

package main

import "github.com/latebit/demarkus/server/internal/config"

func startCertReloader(_ *config.Config, _ bool) {
	// SIGHUP is not available on Windows. Certificate reload requires a server restart.
}
