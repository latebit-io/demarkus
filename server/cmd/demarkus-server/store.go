package main

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/config"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// backend is an opened document store plus the LOOKUP index that belongs to
// it. Close is nil when the backend owns no resources.
type backend struct {
	Store   handler.DocumentStore
	Catalog handler.LookupCatalog
	Close   func() error
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

// openStore opens the configured backend, naming what this build supports
// when the requested one was not compiled in.
func openStore(cfg *config.Config, logger *slog.Logger) (backend, error) {
	open, ok := storeOpeners[cfg.StoreBackend]
	if !ok {
		return backend{}, fmt.Errorf("store backend %q is not available in this build (have: %s)",
			cfg.StoreBackend, strings.Join(storeNames(), ", "))
	}
	return open(cfg, logger)
}

// openFileStore serves documents from versioned files under the content dir.
func openFileStore(cfg *config.Config, logger *slog.Logger) (backend, error) {
	// Open migrates any legacy-layout version files; serving an unmigrated
	// root would hide every legacy document, so failure is fatal.
	s, err := store.Open(cfg.ContentDir)
	if err != nil {
		return backend{}, fmt.Errorf("store open failed: %w", err)
	}
	// A broken walk is fatal (an empty index would fail every hash FETCH); a
	// walk that merely skipped entries is logged per entry and served.
	if err := s.BuildHashIndex(); err != nil && !logPartialWalk(logger, "hash index", err) {
		return backend{}, fmt.Errorf("hash index build failed: %w", err)
	}
	logger.Info("content hash index built", "entries", s.HashIndexSize())
	return backend{Store: s, Catalog: buildCatalog(s, logger)}, nil
}
