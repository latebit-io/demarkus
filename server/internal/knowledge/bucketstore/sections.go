package bucketstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// indexSections fills the snapshot's section index: unchanged bodies carry
// over from previous by hash, fresh bodies index directly, the rest are read
// in parallel. A body that fails to load is logged and skipped (ADR 0012).
func (store *Store) indexSections(ctx context.Context, loaded, previous *snapshot, fresh map[string][]byte, workers int) error {
	var pending []string
	for path, entry := range loaded.Paths {
		if entry.Archived {
			continue
		}
		if body, ok := fresh[path]; ok {
			loaded.Catalog.SetSections(path, catalog.IndexSections(body))
			continue
		}
		if doc := carriedSections(previous, path, entry.BodyHash); doc != nil {
			loaded.Catalog.SetSections(path, doc)
			continue
		}
		pending = append(pending, path)
	}
	return runParallel(ctx, workers, pending, func(ctx context.Context, path string) error {
		view := &readView{ctx: ctx, cancel: func() {}, objects: store.objects, snapshot: loaded}
		document, err := view.get(path, 0)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("index sections %s: %w", path, err)
			}
			store.logger.Warn("section index skipped document", "path", path, "error", err)
			return nil
		}
		loaded.Catalog.SetSections(path, catalog.IndexSections(document.Content))
		return nil
	})
}

// carriedSections returns the previous snapshot's index for path when the
// current body is the same bytes, else nil.
func carriedSections(previous *snapshot, path, bodyHash string) *catalog.DocSections {
	if previous == nil {
		return nil
	}
	entry, ok := previous.Paths[path]
	if !ok || entry.Archived || entry.BodyHash != bodyHash {
		return nil
	}
	return previous.Catalog.Sections(path)
}
