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
	// One walk fills the index and the catalog. A broken walk is fatal (an
	// empty index would hide every document from LIST); a walk that merely
	// skipped entries is logged per entry and served.
	cat := catalog.New()
	err := documents.BuildIndexObserved(func(d storefmt.CurrentDoc) {
		cat.Put(d.Path, d.Metadata, d.Body, d.Modified)
	})
	if err != nil && !logPartialWalk(logger, err) {
		return nil, fmt.Errorf("document index build failed: %w", err)
	}
	logger.Info("document index built", "entries", documents.IndexSize())
	store, err := Open(documents, cat, opts)
	if err != nil {
		return nil, fmt.Errorf("change journal: %w", err)
	}
	return store, nil
}

// logPartialWalk reports each entry a completed walk skipped and returns
// whether err was a partial walk at all.
func logPartialWalk(logger *slog.Logger, err error) bool {
	var partial *storefmt.PartialWalkError
	if !errors.As(err, &partial) {
		return false
	}
	for _, e := range partial.Skipped {
		logger.Warn("document index skipped entry", "path", e.Path, "error", e.Err)
	}
	if partial.Total > len(partial.Skipped) {
		logger.Warn("document index skipped more entries than listed", "total", partial.Total, "listed", len(partial.Skipped))
	}
	return true
}
