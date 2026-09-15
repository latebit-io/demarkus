package bucketstore

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// ExportDocs reads every retained version from one immutable root snapshot.
func ExportDocs(ctx context.Context, objects blob.Store, worldID string, workers int, fn func(string, protocolstore.StoredDocument) error) error {
	if ctx == nil {
		return fmt.Errorf("export bucket store: %w: context is nil", blob.ErrPrecondition)
	}
	if nilStore(objects) {
		return fmt.Errorf("export bucket store: %w: blob store is nil", blob.ErrPrecondition)
	}
	if !validWorldID(worldID) {
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
	loaded, err := loadRootSnapshot(ctx, objects, worldID, workers)
	if err != nil {
		return fmt.Errorf("export bucket store: %w", err)
	}
	view := readView{ctx: ctx, objects: objects, snapshot: loaded}
	paths := slices.Collect(maps.Keys(loaded.Paths))
	sort.Slice(paths, func(i, j int) bool {
		return slices.Compare(strings.Split(paths[i], "/"), strings.Split(paths[j], "/")) < 0
	})
	for _, path := range paths {
		entry := loaded.Paths[path]
		history, err := view.loadHistory(&entry)
		if err != nil {
			return fmt.Errorf("export %s: %w", path, err)
		}
		versions := make([]protocolstore.StoredVersion, len(history.versions))
		for index := range history.versions {
			retained := &history.versions[index]
			raw, err := view.loadBlob(retained.entry.Blob)
			if err != nil {
				return fmt.Errorf("export %s v%d: %w", path, retained.entry.Version, err)
			}
			if _, err := validateStoredDocument(raw, retained); err != nil {
				return fmt.Errorf("export %s v%d: %w", path, retained.entry.Version, err)
			}
			versions[index] = protocolstore.StoredVersion{Version: retained.entry.Version, Stored: bytes.Clone(raw), Modified: retained.modified}
		}
		document := protocolstore.StoredDocument{Versions: versions, Archived: entry.Archived}
		if _, err := protocolstore.ValidateImport(path, document); err != nil {
			return fmt.Errorf("export %s: %w", path, err)
		}
		if err := fn(path, document); err != nil {
			return fmt.Errorf("export %s: %w", path, err)
		}
	}
	return ctx.Err()
}
