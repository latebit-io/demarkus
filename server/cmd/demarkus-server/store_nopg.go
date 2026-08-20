//go:build !pg

package main

import (
	"errors"
	"log/slog"
)

// The default binary stays free of external database dependencies; the
// Postgres flavor is built with -tags pg (see the Makefile's server-pg).
func openPostgresStore(_ string, _ *slog.Logger) (pgBackend, error) {
	return nil, errors.New("this build has no postgres support: use the demarkus-server-pg binary (go build -tags pg)")
}
