package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// backend is an opened document store. Close is nil when the backend owns no
// resources. Changes is the hub the store's commits feed under its durable
// sequence (backend.ChangeSource), so a watcher resumes across a restart.
type backend struct {
	Store   handler.DocumentStore
	Close   func() error
	Changes *changefeed.Hub
}

// storeOpeners holds one opener per compiled-in backend, keyed by the value
// of -store. Every backend registers the same way, so a build carries only
// the backends compiled into it.
var storeOpeners = map[string]func(*config.Config, *slog.Logger) (backend, error){}

func init() { registerStore("file", openFileStore) }

// registerStore adds a backend. Called from init().
func registerStore(name string, open func(*config.Config, *slog.Logger) (backend, error)) {
	storeOpeners[name] = open
}

// storeNames lists the backends this build carries, in sorted order.
func storeNames() []string {
	names := make([]string, 0, len(storeOpeners))
	for name := range storeOpeners {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// gated is the write gate every soul write passes, whatever the backend: the
// content scan (secrets warn for one release, personal data warns) and the
// policy document when the soul publishes one. No seed, no floor.
func gated(b backend) backend {
	b.Store = writepolicy.Enforce(b.Store, writepolicy.Options{Content: writepolicy.ContentOptions{
		SecretsWarn: true, Options: contentcheck.Options{PII: contentcheck.Warn},
	}})
	return b
}

// openStore opens the configured backend, naming what this build supports
// when the requested one was not compiled in.
func openStore(cfg *config.Config, logger *slog.Logger) (backend, error) {
	open, ok := storeOpeners[cfg.StoreBackend]
	if !ok {
		return backend{}, fmt.Errorf("store backend %q is not available in this build (have: %s)",
			cfg.StoreBackend, strings.Join(storeNames(), ", "))
	}
	b, err := open(cfg, logger)
	if err != nil {
		return backend{}, err
	}
	return gated(b), nil
}

// openFileStore serves documents from versioned files under the content dir.
func openFileStore(cfg *config.Config, logger *slog.Logger) (backend, error) {
	s := store.New(cfg.ContentDir)
	// A broken walk is fatal (an empty index would hide every document from
	// LIST); a walk that merely skipped entries is logged per entry and served.
	if err := s.BuildIndex(); err != nil && !logPartialWalk(logger, "document index", err) {
		return backend{}, fmt.Errorf("document index build failed: %w", err)
	}
	logger.Info("document index built", "entries", s.IndexSize())
	fs, err := filestore.Open(s, buildCatalog(s, logger), filestore.Options{ChangeRing: changefeed.DefaultRingSize, Logger: logger})
	if err != nil {
		return backend{}, fmt.Errorf("change journal: %w", err)
	}
	return backend{Store: fs, Close: fs.Close, Changes: fs.Changes()}, nil
}
