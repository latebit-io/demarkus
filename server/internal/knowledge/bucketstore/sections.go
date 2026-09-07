package bucketstore

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// indexSections fills the snapshot's section index for LOOKUP body match.
// An unchanged body carries its index over from previous by body hash, a
// body the writer has in hand is indexed directly, and the rest are read
// from the bucket with workers in parallel. Archived documents are skipped.
// A body that fails to load is logged and left out of body match, so one
// corrupt object degrades recall rather than refusing the snapshot; FETCH
// still reports the corruption. Cancellation is fatal.
func indexSections(ctx context.Context, objects blob.Store, logger *slog.Logger, loaded, previous *snapshot, fresh map[string][]byte, workers int) error {
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
	if len(pending) == 0 {
		return nil
	}
	sort.Strings(pending)
	if workers < 1 {
		return fmt.Errorf("%w: section workers must be positive", blob.ErrPrecondition)
	}

	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan string, len(pending))
	for _, path := range pending {
		jobs <- path
	}
	close(jobs)
	var wait sync.WaitGroup
	for range min(workers, len(pending)) {
		wait.Go(func() {
			view := &readView{ctx: workCtx, cancel: func() {}, objects: objects, snapshot: loaded}
			for path := range jobs {
				if workCtx.Err() != nil {
					return
				}
				document, err := view.get(path, 0)
				if err != nil {
					if workCtx.Err() != nil {
						cancel(fmt.Errorf("index sections %s: %w", path, err))
						return
					}
					logger.Warn("section index skipped document", "path", path, "error", err)
					continue
				}
				loaded.Catalog.SetSections(path, catalog.IndexSections(document.Content))
			}
		})
	}
	wait.Wait()
	if cause := context.Cause(workCtx); cause != nil {
		return cause
	}
	return nil
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
