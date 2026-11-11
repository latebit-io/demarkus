package filestore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	protocolstore "github.com/latebit-io/demarkus/server/store"
)

// OpenDir serves the versioned documents under dir: index, lookup catalog,
// and WATCH when opts.ChangeRing is set. A writable dir is created, a
// read-only one must exist. Both servers open a directory through it.
func OpenDir(dir string, opts Options) (*Store, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	logger := opts.Logger
	if !opts.ReadOnly {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store directory: %w", err)
		}
	}
	documents := protocolstore.New(dir)
	// Read before the walk: a write landing during it moves the marker and
	// the first read walks again.
	generation, err := documents.Generation()
	if err != nil {
		return nil, err
	}
	cat, skipped, err := buildCatalog(documents, logger)
	if err != nil {
		return nil, err
	}
	store, err := Open(documents, cat, opts)
	if err != nil {
		return nil, fmt.Errorf("change journal: %w", err)
	}
	store.unindexed = skipped
	store.generation.Store(&generation)
	return store, nil
}

// buildCatalog fills the index and a new catalog from one walk. A broken walk
// is fatal (an empty index would hide every document from LIST); a walk that
// merely skipped entries is logged per entry and served.
func buildCatalog(documents *protocolstore.Store, logger *slog.Logger) (cat *catalog.Catalog, skipped int, err error) {
	cat = catalog.New()
	err = documents.BuildIndexObserved(func(d storefmt.CurrentDoc) {
		cat.Put(d.Path, d.Metadata, d.Body, d.Modified)
	})
	skipped, partial := logPartialWalk(logger, err)
	if err != nil && !partial {
		return nil, 0, fmt.Errorf("document index build failed: %w", err)
	}
	logger.Info("document index built", "entries", documents.IndexSize(), "skipped", skipped)
	return cat, skipped, nil
}

// reindexIfChanged rebuilds the index and catalog when a writer outside this
// process moved the generation marker (protocolstore.Store.MarkChanged).
// Watchers resync: the changes carry no events.
func (s *Store) reindexIfChanged(ctx context.Context) error {
	generation, err := s.documents.Generation()
	if err != nil {
		return fmt.Errorf("filestore: %w", err)
	}
	if generation == *s.generation.Load() {
		return nil
	}
	if err := s.mu.Lock(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if generation == *s.generation.Load() {
		return nil
	}
	cat, skipped, err := buildCatalog(s.documents, s.logger)
	if err != nil {
		return fmt.Errorf("filestore: content changed outside the server: %w", err)
	}
	s.catalog, s.unindexed = cat, skipped
	s.generation.Store(&generation)
	if s.changes != nil {
		s.changes.Skip(s.changes.Head().Seq + 1)
	}
	s.logger.Info("content changed outside the server; index and catalog rebuilt", "generation", generation)
	return nil
}

// logPartialWalk reports each entry a completed walk skipped and returns how
// many, and whether err was a partial walk at all.
func logPartialWalk(logger *slog.Logger, err error) (skipped int, partial bool) {
	var walk *storefmt.PartialWalkError
	if !errors.As(err, &walk) {
		return 0, false
	}
	for _, e := range walk.Skipped {
		logger.Warn("document index skipped entry", "path", e.Path, "error", e.Err)
	}
	if walk.Total > len(walk.Skipped) {
		logger.Warn("document index skipped more entries than listed", "total", walk.Total, "listed", len(walk.Skipped))
	}
	return walk.Total, true
}
