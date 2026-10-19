package filestore

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/catalog"
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
	// A broken walk is fatal (an empty index would hide every document from
	// LIST); a walk that merely skipped entries is logged per entry and served.
	if err := documents.BuildIndex(); err != nil && !logPartialWalk(logger, "document index", err) {
		return nil, fmt.Errorf("document index build failed: %w", err)
	}
	logger.Info("document index built", "entries", documents.IndexSize())
	store, err := Open(documents, buildCatalog(documents, logger), opts)
	if err != nil {
		return nil, fmt.Errorf("change journal: %w", err)
	}
	return store, nil
}

// buildCatalog builds the LOOKUP catalog from a walk of current documents.
// A failed walk leaves an empty catalog rather than aborting startup.
func buildCatalog(documents *protocolstore.Store, logger *slog.Logger) *catalog.Catalog {
	cat := catalog.New()
	err := documents.WalkCurrent(func(d storefmt.CurrentDoc) error {
		cat.Put(d.Path, d.Metadata, d.Body, d.Modified)
		return nil
	})
	if err != nil && !logPartialWalk(logger, "lookup catalog", err) {
		logger.Warn("lookup catalog build failed", "error", err)
		return cat
	}
	logger.Info("lookup catalog built", "entries", cat.Len())
	return cat
}

// logPartialWalk reports each entry a completed walk skipped and returns
// whether err was a partial walk at all.
func logPartialWalk(logger *slog.Logger, what string, err error) bool {
	var partial *storefmt.PartialWalkError
	if !errors.As(err, &partial) {
		return false
	}
	for _, e := range partial.Skipped {
		logger.Warn(what+" skipped entry", "path", e.Path, "error", e.Err)
	}
	if partial.Total > len(partial.Skipped) {
		logger.Warn(what+" skipped more entries than listed", "total", partial.Total, "listed", len(partial.Skipped))
	}
	return true
}
