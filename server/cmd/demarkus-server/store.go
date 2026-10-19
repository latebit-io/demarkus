package main

import (
	"log/slog"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// openStore serves the versioned documents under the content dir; the
// config's validation already pins the one backend this build carries.
func openStore(cfg *config.Config, logger *slog.Logger) (*filestore.Store, error) {
	return filestore.OpenDir(cfg.ContentDir, filestore.Options{
		ChangeRing: changefeed.DefaultRingSize,
		ReadOnly:   cfg.ReadOnly,
		Logger:     logger,
	})
}

// gated is the write gate every soul write passes: the content scan (a
// secret is refused, personal data warns) and the policy document when the
// soul publishes one. No seed, no floor.
func gated(store backend.Store) backend.Store {
	return writepolicy.Enforce(store, writepolicy.Options{Content: writepolicy.ContentOptions{
		Options: contentcheck.Options{PII: contentcheck.Warn},
	}})
}
