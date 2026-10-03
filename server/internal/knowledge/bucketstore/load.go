package bucketstore

import (
	"context"
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

// readMarker checks the marker names this world.
func readMarker(ctx context.Context, objects blob.Store, worldID string) error {
	object, err := objects.Get(ctx, markerKey)
	if err != nil {
		return fmt.Errorf("read world marker: %w", err)
	}
	if err := validateReadObject(markerKey, &object); err != nil {
		return err
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
	newest, err := newestCheckpointSequence(ctx, objects, 0)
	if err != nil {
		return checkpointObject{}, err
	}
	return readCheckpoint(ctx, objects, worldID, newest)
}

// readCheckpoint reads and checks the checkpoint at sequence.
func readCheckpoint(ctx context.Context, objects blob.Store, worldID string, sequence int64) (checkpointObject, error) {
	checkpoint, err := getValidated(ctx, objects, checkpointKey(sequence), func(checkpoint *checkpointObject) error {
		return validateCheckpoint(checkpoint, sequence)
	})
	if err != nil {
		return checkpointObject{}, fmt.Errorf("load checkpoint %d: %w", sequence, err)
	}
	if checkpoint.WorldID != worldID {
		return checkpointObject{}, fmt.Errorf("%w: checkpoint %d belongs to world %q", blob.ErrIntegrity, sequence, checkpoint.WorldID)
	}
	return checkpoint, nil
}

// newestCheckpointSequence lists the checkpoints after a known one, or all
// of them from 0, for the highest sequence.
func newestCheckpointSequence(ctx context.Context, objects blob.Store, after int64) (int64, error) {
	newest := after
	for page, err := range sequencedPages(ctx, objects, checkpointPrefix, after) {
		if err != nil {
			return 0, err
		}
		newest = max(newest, page[len(page)-1].sequence)
	}
	if newest == 0 {
		return 0, fmt.Errorf("%w: world has a marker but no checkpoint", blob.ErrIntegrity)
	}
	return newest, nil
}

// loadCheckpoint builds the snapshot a checkpoint's root holds.
func loadCheckpoint(ctx context.Context, objects blob.Store, checkpoint checkpointObject, workers int) (*snapshot, error) {
	root, err := loadRoot(ctx, objects, checkpoint)
	if err != nil {
		return nil, fmt.Errorf("load root: %w", err)
	}
	shards, err := shardReader{objects: objects, layout: root.layout, workers: workers}.states(ctx, indexes(len(root.layout.Shards)))
	if err != nil {
		return nil, err
	}
	loaded := newSnapshot()
	loaded.Sequence, loaded.Tip, loaded.Checkpoint = checkpoint.Sequence, checkpoint.Tip, root.layout
	for _, states := range shards {
		for _, state := range states {
			if loaded.path(state.Path) != nil {
				return nil, fmt.Errorf("%w: checkpoint holds %q twice", blob.ErrIntegrity, state.Path)
			}
			loaded.put(nil, state)
		}
	}
	if loaded.Paths.Len() != root.documents {
		return nil, fmt.Errorf("%w: root document count is %d, loaded %d paths", blob.ErrIntegrity, root.documents, loaded.Paths.Len())
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

// rootRead is a checkpoint's root: its key, shard layout and document count.
type rootRead struct {
	Key       string
	layout    *checkpointBase
	documents int
}

// loadRoot reads a checkpoint's root.
func loadRoot(ctx context.Context, objects objectGetter, checkpoint checkpointObject) (rootRead, error) {
	key := rootKey(checkpoint.Root.Hash)
	root, err := getImmutable(ctx, objects, keyedRef{checkpoint.Root, key}, func(root *foldedRoot) error {
		return validateFoldedRoot(root, checkpoint.WorldID)
	})
	if err != nil {
		return rootRead{}, err
	}
	layout := &checkpointBase{Sequence: checkpoint.Sequence, Bits: root.ShardBits, Shards: root.Shards}
	return rootRead{Key: checkpoint.Root.Key, layout: layout, documents: root.DocumentCount}, nil
}

// shardReader reads a checkpoint's shards, workers at a time.
type shardReader struct {
	objects objectGetter
	layout  *checkpointBase
	workers int
}

// states reads the given shards and returns their documents, shard by shard.
func (reader shardReader) states(ctx context.Context, shards []int) ([][]*pathState, error) {
	loaded := make([][]*pathState, len(shards))
	err := runParallel(ctx, reader.workers, indexes(len(shards)), func(ctx context.Context, position int) error {
		index := shards[position]
		states, err := reader.folded(ctx, index)
		if err != nil {
			return fmt.Errorf("load shard %s: %w", reader.layout.Shards[index].Shard, err)
		}
		loaded[position] = states
		return nil
	})
	if err != nil {
		return nil, err
	}
	return loaded, nil
}

// shard reads and checks the shard at index.
func (reader shardReader) shard(ctx context.Context, index int) (foldedShard, error) {
	bits := reader.layout.Bits
	label, ref := shardLabel(index, bits), reader.layout.Shards[index]
	return getImmutable(ctx, reader.objects, keyedRef{ref.objectRef, shardKey(label, ref.Hash)}, func(shard *foldedShard) error {
		return validateFoldedShard(shard, index, bits)
	})
}

func (reader shardReader) folded(ctx context.Context, index int) ([]*pathState, error) {
	shard, err := reader.shard(ctx, index)
	if err != nil {
		return nil, err
	}
	states := make([]*pathState, len(shard.Entries))
	for entryIndex := range shard.Entries {
		entry := &shard.Entries[entryIndex]
		if states[entryIndex], err = foldedState(entry); err != nil {
			return nil, fmt.Errorf("%w: checkpoint path %q: %v", blob.ErrIntegrity, entry.Path, err)
		}
	}
	return states, nil
}

// foldedState is a document as a folded shard entry records it.
func foldedState(entry *foldedEntry) (*pathState, error) {
	base, err := foldedBase(entry)
	if err != nil {
		return nil, err
	}
	record, err := catalogEntry(&entry.Catalog)
	if err != nil {
		return nil, err
	}
	return &pathState{
		Path: entry.Path, Current: entry.Current, First: base.first(), Archived: entry.Archived,
		BodyHash: entry.BodyHash, Modified: base.Modified, Entry: record, Base: base,
		hashPrefix: hashPrefix(entry.PathHash),
	}, nil
}

// foldedBase is a checkpoint entry's base alone, without its catalog entry.
func foldedBase(entry *foldedEntry) (*baseEntry, error) {
	modified, err := parseTimestamp(entry.Modified)
	if err != nil {
		return nil, err
	}
	return &baseEntry{
		History: entry.History, Current: entry.Current, Archived: entry.Archived,
		BodyHash: entry.BodyHash, Modified: modified,
	}, nil
}

// applyError is a slot that failed to apply. The snapshot may then hold part
// of it, so nothing built on it is served.
type applyError struct{ error }

func (err *applyError) Unwrap() error { return err.error }

// replayOptions names the world, the read parallelism, and who hears of each
// applied slot.
type replayOptions struct {
	worldID string
	workers int
	onSlot  func(slot *slotObject)
}

// replay applies every slot after loaded's sequence, a page of names at a
// time, reading each page's slots in parallel.
func replay(ctx context.Context, objects blob.Store, loaded *snapshot, options replayOptions) error {
	for page, err := range sequencedPages(ctx, objects, logPrefix, loaded.Sequence) {
		if err != nil {
			return err
		}
		slots := make([]slotRead, len(page))
		err := runParallel(ctx, options.workers, indexes(len(page)), func(ctx context.Context, index int) error {
			read, err := readSlot(ctx, objects, options.worldID, page[index].sequence)
			if errors.Is(err, blob.ErrNotFound) {
				return fmt.Errorf("%w: listed slot %d is missing: %w", blob.ErrIntegrity, page[index].sequence, err)
			}
			slots[index] = read
			return err
		})
		if err != nil {
			return fmt.Errorf("replay: %w", err)
		}
		for _, read := range slots {
			if err := loaded.applySlot(read); err != nil {
				return &applyError{fmt.Errorf("replay: %w", err)}
			}
			if options.onSlot != nil {
				options.onSlot(read.slot)
			}
		}
	}
	return nil
}

// sequenced is a listed object named by its sequence.
type sequenced struct {
	blob.Attributes
	sequence int64
}

// sequencedPages yields, a page at a time, the objects a checkpoint or slot
// listing names after the given sequence; an error ends it.
func sequencedPages(ctx context.Context, objects blob.Store, prefix string, after int64) iter.Seq2[[]sequenced, error] {
	return func(yield func([]sequenced, error) bool) {
		startAfter := ""
		if after > 0 {
			startAfter = fmt.Sprintf("%s%016x.json", prefix, after)
		}
		for page, err := range attributePages(ctx, objects, prefix, startAfter) {
			if err != nil {
				yield(nil, err)
				return
			}
			named := make([]sequenced, len(page))
			for index, attributes := range page {
				sequence, ok := sequenceOfKey(attributes.Key, prefix)
				if !ok {
					yield(nil, fmt.Errorf("%w: %q is not a sequence name", blob.ErrIntegrity, attributes.Key))
					return
				}
				named[index] = sequenced{Attributes: attributes, sequence: sequence}
			}
			if !yield(named, nil) {
				return
			}
		}
	}
}

// attributePages lists prefix after startAfter, one non-empty page at a time.
func attributePages(ctx context.Context, objects blob.Store, prefix, startAfter string) iter.Seq2[[]blob.Attributes, error] {
	return func(yield func([]blob.Attributes, error) bool) {
		cursor := ""
		for {
			page, err := objects.List(ctx, prefix, startAfter, cursor)
			if err != nil {
				yield(nil, fmt.Errorf("list %q: %w", prefix, err))
				return
			}
			if len(page.Objects) > 0 && !yield(page.Objects, nil) || page.NextCursor == "" {
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
func getValidated[T any](ctx context.Context, objects blob.Store, key string, validate func(*T) error) (T, error) {
	object, err := objects.Get(ctx, key)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("read %q: %w", key, err)
	}
	return decodeValidated(key, &object, validate)
}

// decodeValidated checks an object read at key and decodes it.
func decodeValidated[T any](key string, object *blob.Object, validate func(*T) error) (value T, err error) {
	if err := validateReadObject(key, object); err != nil {
		return value, err
	}
	return decodeChecked(key, object.Data, validate)
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

func getImmutable[T any](ctx context.Context, objects objectGetter, keyed keyedRef, validate func(*T) error) (T, error) {
	data, err := getVerified(ctx, objects, keyed)
	if err != nil {
		var zero T
		return zero, err
	}
	return decodeChecked(keyed.Key, data, validate)
}

// getVerified reads a referenced object and checks its bytes hash to it.
func getVerified(ctx context.Context, objects objectGetter, keyed keyedRef) ([]byte, error) {
	data, err := getReferenced(ctx, objects, keyed)
	if err == nil {
		err = verifyBlobHash(keyed.objectRef, data)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// getReferenced is getVerified without the hash check, for a caller that
// verifies the bytes another way: a missing object is an integrity failure.
func getReferenced(ctx context.Context, objects objectGetter, keyed keyedRef) ([]byte, error) {
	ref := keyed.objectRef
	if err := verifyRef(ref, keyed.expectedKey); err != nil {
		return nil, fmt.Errorf("%w: %v", blob.ErrIntegrity, err)
	}
	value, err := objects.Get(ctx, ref.Key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("%w: referenced object %q is missing: %w", blob.ErrIntegrity, ref.Key, err)
		}
		return nil, fmt.Errorf("read referenced object %q: %w", ref.Key, err)
	}
	if err := validateReadObject(ref.Key, &value); err != nil {
		return nil, err
	}
	return value.Data, nil
}

// decodeChecked decodes the canonical bytes read at key and validates them.
func decodeChecked[T any](key string, data []byte, validate func(*T) error) (T, error) {
	var result T
	if err := decodeImmutable(data, &result); err != nil {
		return result, fmt.Errorf("%w: decode object %q: %v", blob.ErrIntegrity, key, err)
	}
	if err := validate(&result); err != nil {
		return result, fmt.Errorf("%w: validate object %q: %v", blob.ErrIntegrity, key, err)
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
