//go:build pg

package main

import (
	"fmt"
	"log/slog"

	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/pgstore"
)

// The Postgres backend is compiled in only with -tags pg. This file is the
// whole seam: deleting it, internal/pgstore and cmd/demarkus-migrate removes
// the backend, and nothing in the default build refers to it.
func init() { registerStore("postgres", openPostgresStore) }

func openPostgresStore(cfg *config.Config, logger *slog.Logger) (backend, error) {
	ps, err := pgstore.Open(cfg.PostgresDSN, logger)
	if err != nil {
		return backend{}, fmt.Errorf("postgres store setup failed: %w", err)
	}
	// LOOKUP is DB-backed; a per-process index would diverge across pods.
	return backend{Store: ps, Catalog: ps, Close: ps.Close}, nil
}
