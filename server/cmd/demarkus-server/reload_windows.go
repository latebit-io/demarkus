//go:build windows

package main

import (
	"log/slog"

	"github.com/latebit-io/demarkus/server/internal/config"
)

func startCertReloader(_ *config.Config, _ bool, _ tokenReloader, _ *slog.Logger) {
	// SIGHUP is not available on Windows. Certificate reload requires a server restart.
}
