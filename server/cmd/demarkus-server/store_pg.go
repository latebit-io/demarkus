//go:build pg

package main

import (
	"fmt"
	"log/slog"

	"github.com/latebit-io/demarkus/server/internal/pgstore"
)

// openPostgresStore is the only edge between the server binary and pgstore.
// Deleting the backend means deleting this file, internal/pgstore,
// cmd/demarkus-migrate, and running go mod tidy.
func openPostgresStore(dsn string, logger *slog.Logger) (pgBackend, error) {
	ps, err := pgstore.Open(dsn, logger)
	if err != nil {
		return nil, fmt.Errorf("postgres store setup failed: %w", err)
	}
	return ps, nil
}
