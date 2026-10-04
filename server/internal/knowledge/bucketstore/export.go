package bucketstore

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
)

// ExportOptions names the world to export and how many shards load at once.
type ExportOptions struct {
	WorldID string
	Workers int
}

// ExportDocs reads every retained version from one snapshot of the log.
func ExportDocs(ctx context.Context, objects blob.Store, options ExportOptions, fn func(string, storefmt.StoredDocument) error) error {
	worldID, workers := options.WorldID, options.Workers
	if ctx == nil {
		return fmt.Errorf("export bucket store: %w: context is nil", blob.ErrPrecondition)
	}
	if nilStore(objects) {
		return fmt.Errorf("export bucket store: %w: blob store is nil", blob.ErrPrecondition)
	}
	if !validUUID(worldID) {
		return fmt.Errorf("export bucket store: %w: invalid world ID %q", blob.ErrPrecondition, worldID)
	}
	if workers < 0 {
		return fmt.Errorf("export bucket store: %w: workers must not be negative", blob.ErrPrecondition)
	}
	if fn == nil {
		return fmt.Errorf("export bucket store: %w: callback is nil", blob.ErrPrecondition)
	}
	if workers == 0 {
		workers = defaultShardWorkers
	}
	loaded, err := loadBase(ctx, objects, worldID, workers)
	if err == nil {
		err = replay(ctx, objects, loaded, replayOptions{worldID: worldID, workers: workers})
	}
	if err != nil {
		return fmt.Errorf("export bucket store: %w", err)
	}
	view := readView{objects: objects, snapshot: loaded}
	entries := make([]*pathState, 0, loaded.Paths.Len())
	loaded.Paths.Ascend(func(state *pathState) bool {
		entries = append(entries, state)
		return true
	})
	slices.SortFunc(entries, func(a, b *pathState) int {
		return slices.Compare(strings.Split(a.Path, "/"), strings.Split(b.Path, "/"))
	})
	for _, entry := range entries {
		path := entry.Path
		history, err := view.history(ctx, entry)
		if err != nil {
			return fmt.Errorf("export %s: %w", path, err)
		}
		versions := make([]storefmt.StoredVersion, len(history))
		for index := range history {
			retained := &history[index]
			raw, _, err := view.loadStored(ctx, retained)
			if err != nil {
				return fmt.Errorf("export %s v%d: %w", path, retained.entry.Version, err)
			}
			versions[index] = storefmt.StoredVersion{Version: retained.entry.Version, Stored: bytes.Clone(raw), Modified: retained.modified}
		}
		document := storefmt.StoredDocument{Versions: versions, Archived: entry.Archived}
		if err := storefmt.ValidateImport(path, document); err != nil {
			return fmt.Errorf("export %s: %w", path, err)
		}
		if err := fn(path, document); err != nil {
			return fmt.Errorf("export %s: %w", path, err)
		}
	}
	return ctx.Err()
}
