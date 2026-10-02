package bucketstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"

	"github.com/latebit-io/demarkus/server/blob"
)

// loadBase reads the world marker and builds the snapshot its newest
// checkpoint holds.
func loadBase(ctx context.Context, objects blob.Store, worldID string, workers int) (*snapshot, error) {
	if err := readMarker(ctx, objects, worldID); err != nil {
		return nil, err
	}
	checkpoint, err := newestCheckpoint(ctx, objects, worldID)
	if err != nil {
		return nil, err
	}
	return loadCheckpoint(ctx, objects, checkpoint, workers)
}

// readMarker checks the marker names this world under schema 2. A schema 1
// head is a world that has not migrated yet.
func readMarker(ctx context.Context, objects blob.Store, worldID string) error {
	object, err := objects.Get(ctx, markerKey)
	if err != nil {
		return fmt.Errorf("read world marker: %w", err)
	}
	if err := validateReadObject(markerKey, &object); err != nil {
		return err
	}
	var version struct {
		Schema int `json:"schema"`
	}
	if json.Unmarshal(object.Data, &version) == nil && version.Schema == schemaVersion {
		return fmt.Errorf("%w: world head is schema %d; it opens once migrated to the commit log (ADR 0036)", blob.ErrPrecondition, version.Schema)
	}
	var marker markerObject
	if err := decodeImmutable(object.Data, &marker); err != nil {
		return fmt.Errorf("%w: decode world marker: %v", blob.ErrIntegrity, err)
	}
	if err := validateMarker(&marker); err != nil {
		return fmt.Errorf("%w: validate world marker: %v", blob.ErrIntegrity, err)
	}
	if marker.WorldID != worldID {
		return fmt.Errorf("%w: configured world ID %q does not match marker world ID %q", blob.ErrPrecondition, worldID, marker.WorldID)
	}
	return nil
}

// newestCheckpoint lists the checkpoints and reads the one with the highest
// sequence; the marker is written after checkpoint zero, so one must exist.
func newestCheckpoint(ctx context.Context, objects blob.Store, worldID string) (checkpointObject, error) {
	newest := int64(0)
	for sequences, err := range sequencePages(ctx, objects, checkpointPrefix, 0) {
		if err != nil {
			return checkpointObject{}, err
		}
		newest = max(newest, sequences[len(sequences)-1])
	}
	if newest == 0 {
		return checkpointObject{}, fmt.Errorf("%w: world has a marker but no checkpoint", blob.ErrIntegrity)
	}
	checkpoint, _, err := getValidated(ctx, objects, checkpointKey(newest), func(checkpoint *checkpointObject) error {
		return validateCheckpoint(checkpoint, newest)
	})
	if err != nil {
		return checkpointObject{}, fmt.Errorf("load checkpoint: %w", err)
	}
	if checkpoint.WorldID != worldID {
		return checkpointObject{}, fmt.Errorf("%w: checkpoint %d belongs to world %q", blob.ErrIntegrity, newest, checkpoint.WorldID)
	}
	return checkpoint, nil
}

// loadCheckpoint builds the snapshot a checkpoint's root holds.
func loadCheckpoint(ctx context.Context, objects blob.Store, checkpoint checkpointObject, workers int) (*snapshot, error) {
	root, err := getImmutable(ctx, objects, keyedRef{checkpoint.Root, rootKey(checkpoint.Root.Hash)}, func(root *rootObject) error {
		return validateRootObject(root, checkpoint.WorldID)
	})
	if err != nil {
		return nil, fmt.Errorf("load root: %w", err)
	}
	shards, err := loadShards(ctx, objects, root.Shards, workers)
	if err != nil {
		return nil, err
	}
	loaded := newSnapshot()
	loaded.Sequence, loaded.Tip = checkpoint.Sequence, checkpoint.Tip
	for shardIndex := range shards {
		for entryIndex := range shards[shardIndex].Entries {
			entry := &shards[shardIndex].Entries[entryIndex]
			state, err := checkpointState(entry)
			if err != nil {
				return nil, fmt.Errorf("%w: checkpoint path %q: %v", blob.ErrIntegrity, entry.Path, err)
			}
			if loaded.path(entry.Path) != nil {
				return nil, fmt.Errorf("%w: checkpoint holds %q twice", blob.ErrIntegrity, entry.Path)
			}
			loaded.put(nil, state)
		}
	}
	if loaded.Paths.Len() != root.DocumentCount {
		return nil, fmt.Errorf("%w: root document count is %d, loaded %d paths", blob.ErrIntegrity, root.DocumentCount, loaded.Paths.Len())
	}
	// A document is never an ancestor: nothing may lie under one.
	var topology error
	loaded.Paths.Ascend(func(state *pathState) bool {
		if loaded.isDirectory(state.Path) {
			topology = fmt.Errorf("%w: documents lie under document %q", blob.ErrIntegrity, state.Path)
		}
		return topology == nil
	})
	return loaded, topology
}

// checkpointState is a document as a checkpoint's shard entry records it.
func checkpointState(entry *shardEntry) (*pathState, error) {
	modified, err := parseTimestamp(entry.Modified)
	if err != nil {
		return nil, err
	}
	catalogRecord := entry.Catalog
	record, err := catalogEntry(&catalogRecord)
	if err != nil {
		return nil, err
	}
	return &pathState{
		Path:     entry.Path,
		Current:  entry.Current,
		Archived: entry.Archived,
		BodyHash: entry.BodyHash,
		Modified: modified,
		Entry:    record,
		Base: &baseEntry{
			Manifest: entry.Manifest, Current: entry.Current, Archived: entry.Archived,
			BodyHash: entry.BodyHash, Modified: modified,
		},
	}, nil
}

// replayOptions names the world, the read parallelism, who hears of each
// applied slot, and where changed live paths are collected (nil drops them).
type replayOptions struct {
	worldID string
	workers int
	onSlot  func(slot *slotObject)
	reindex map[string]struct{}
}

// replay applies every slot after loaded's sequence, a page of names at a
// time, reading each page's slots in parallel.
func replay(ctx context.Context, objects blob.Store, loaded *snapshot, options replayOptions) error {
	reindex := options.reindex
	if reindex == nil {
		reindex = make(map[string]struct{})
	}
	for firsts, err := range sequencePages(ctx, objects, logPrefix, loaded.Sequence) {
		if err != nil {
			return err
		}
		slots := make([]*slotObject, len(firsts))
		hashes := make([]string, len(firsts))
		err := runParallel(ctx, options.workers, indexes(len(firsts)), func(ctx context.Context, index int) error {
			slot, hash, err := readSlot(ctx, objects, options.worldID, firsts[index])
			if errors.Is(err, blob.ErrNotFound) {
				return fmt.Errorf("%w: listed slot %d is missing: %w", blob.ErrIntegrity, firsts[index], err)
			}
			slots[index], hashes[index] = slot, hash
			return err
		})
		if err != nil {
			return fmt.Errorf("replay: %w", err)
		}
		for index, slot := range slots {
			if err := loaded.applySlot(slot, hashes[index], reindex); err != nil {
				return fmt.Errorf("replay: %w", err)
			}
			if options.onSlot != nil {
				options.onSlot(slot)
			}
		}
	}
	return nil
}

// sequencePages yields, a page at a time, the sequences a checkpoint or slot
// listing names after the given one; an error ends it.
func sequencePages(ctx context.Context, objects blob.Store, prefix string, after int64) iter.Seq2[[]int64, error] {
	return func(yield func([]int64, error) bool) {
		startAfter := ""
		if after > 0 {
			startAfter = fmt.Sprintf("%s%016x.json", prefix, after)
		}
		cursor := ""
		for {
			page, err := objects.List(ctx, prefix, startAfter, cursor)
			if err != nil {
				yield(nil, fmt.Errorf("list %q: %w", prefix, err))
				return
			}
			sequences := make([]int64, len(page.Objects))
			for index, attributes := range page.Objects {
				sequence, ok := sequenceOfKey(attributes.Key, prefix)
				if !ok {
					yield(nil, fmt.Errorf("%w: %q is not a sequence name", blob.ErrIntegrity, attributes.Key))
					return
				}
				sequences[index] = sequence
			}
			if len(sequences) > 0 && !yield(sequences, nil) || page.NextCursor == "" {
				return
			}
			cursor = page.NextCursor
		}
	}
}

func indexes(n int) []int {
	all := make([]int, n)
	for index := range all {
		all[index] = index
	}
	return all
}

// getValidated reads the object at a fixed key and returns it with its
// bytes; a defect in its encoding or content is an integrity failure.
func getValidated[T any](ctx context.Context, objects blob.Store, key string, validate func(*T) error) (value T, data []byte, err error) {
	object, err := objects.Get(ctx, key)
	if err != nil {
		return value, nil, fmt.Errorf("read %q: %w", key, err)
	}
	if err := validateReadObject(key, &object); err != nil {
		return value, nil, err
	}
	if err := decodeImmutable(object.Data, &value); err != nil {
		return value, nil, fmt.Errorf("%w: decode %q: %v", blob.ErrIntegrity, key, err)
	}
	if err := validate(&value); err != nil {
		return value, nil, fmt.Errorf("%w: validate %q: %v", blob.ErrIntegrity, key, err)
	}
	return value, object.Data, nil
}

func loadShards(ctx context.Context, objects blob.Store, refs []shardRef, workers int) (*[shardCount]shardObject, error) {
	loaded := new([shardCount]shardObject)
	err := runParallel(ctx, workers, indexes(shardCount), func(ctx context.Context, index int) error {
		ref := refs[index]
		expectedShard := fmt.Sprintf("%02x", index)
		shard, err := getImmutable(ctx, objects, keyedRef{ref.objectRef, shardKey(expectedShard, ref.Hash)}, func(shard *shardObject) error {
			return validateShardObject(shard, expectedShard)
		})
		if err != nil {
			return fmt.Errorf("load shard %s: %w", expectedShard, err)
		}
		loaded[index] = shard
		return nil
	})
	if err != nil {
		return nil, err
	}
	return loaded, nil
}

// runParallel applies fn to every job on up to workers goroutines. The first
// error cancels the rest and is returned; fn's errors are not retried.
func runParallel[T any](ctx context.Context, workers int, jobs []T, fn func(ctx context.Context, job T) error) error {
	if workers < 1 {
		return fmt.Errorf("%w: workers must be positive", blob.ErrPrecondition)
	}
	if len(jobs) == 0 {
		return nil
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	queue := make(chan T, len(jobs))
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	var wait sync.WaitGroup
	for range min(workers, len(jobs)) {
		wait.Go(func() {
			for job := range queue {
				if workCtx.Err() != nil {
					return
				}
				if err := fn(workCtx, job); err != nil {
					cancel(err)
					return
				}
			}
		})
	}
	wait.Wait()
	return context.Cause(workCtx)
}

// keyedRef is a stored reference and the key its kind and hash must produce.
type keyedRef struct {
	objectRef
	expectedKey string
}

func getImmutable[T any](ctx context.Context, objects blob.Store, keyed keyedRef, validate func(*T) error) (T, error) {
	ref := keyed.objectRef
	var result T
	if err := verifyRef(ref, keyed.expectedKey); err != nil {
		return result, fmt.Errorf("%w: %v", blob.ErrIntegrity, err)
	}
	value, err := objects.Get(ctx, ref.Key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return result, fmt.Errorf("%w: referenced object %q is missing: %w", blob.ErrIntegrity, ref.Key, err)
		}
		return result, fmt.Errorf("read referenced object %q: %w", ref.Key, err)
	}
	if err := validateReadObject(ref.Key, &value); err != nil {
		return result, err
	}
	if actual := hashHex(value.Data); actual != ref.Hash {
		return result, fmt.Errorf("%w: object %q hash is %s, reference is %s", blob.ErrIntegrity, ref.Key, actual, ref.Hash)
	}
	if err := decodeImmutable(value.Data, &result); err != nil {
		return result, fmt.Errorf("%w: decode object %q: %v", blob.ErrIntegrity, ref.Key, err)
	}
	if err := validate(&result); err != nil {
		return result, fmt.Errorf("%w: validate object %q: %v", blob.ErrIntegrity, ref.Key, err)
	}
	return result, nil
}

func validateReadObject(key string, object *blob.Object) error {
	attributes := object.Attributes
	if attributes.Key != key || attributes.Generation <= 0 || attributes.Size != int64(len(object.Data)) || attributes.Modified.IsZero() {
		return fmt.Errorf("%w: object %q has inconsistent attributes", blob.ErrIntegrity, key)
	}
	return nil
}
