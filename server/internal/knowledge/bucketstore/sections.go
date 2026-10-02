package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// indexSections fills the section index of every live path in reindex on a
// snapshot not yet published, reading the bodies in parallel. A body that
// fails to load is logged and skipped (ADR 0012).
func (store *Store) indexSections(ctx context.Context, next *snapshot, reindex map[string]struct{}) error {
	var pending []*pathState
	var mu sync.Mutex
	indexed := make(map[string]*catalog.DocSections, len(reindex))
	for path := range reindex {
		state := next.path(path)
		if state == nil || state.Archived {
			continue
		}
		pending = append(pending, state)
	}
	err := runParallel(ctx, store.shardWorkers, pending, func(ctx context.Context, state *pathState) error {
		view := &readView{objects: store.objects, snapshot: next}
		document, err := view.get(ctx, state.Path, 0)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("index sections %s: %w", state.Path, err)
			}
			store.logger.Warn("section index skipped document", "path", state.Path, "error", err)
			return nil
		}
		sections := catalog.IndexSections(document.Content)
		mu.Lock()
		indexed[state.Path] = sections
		mu.Unlock()
		return nil
	})
	if err != nil {
		return err
	}
	for path, sections := range indexed {
		state := *next.path(path)
		state.Sections = sections
		next.Paths.ReplaceOrInsert(&state)
	}
	return nil
}
