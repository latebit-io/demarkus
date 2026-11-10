package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

// checkpointLoader builds the snapshot of a world's newest checkpoint that
// loads. A set timeout bounds the listing and each checkpoint apart, so a
// fallback gets a whole request's time.
type checkpointLoader struct {
	objects blob.Store
	worldID string
	workers int
	logger  *slog.Logger
	timeout time.Duration
}

func (loader checkpointLoader) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if loader.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, loader.timeout)
}

// loadedBase is the snapshot of the checkpoint that loaded, and the reach a
// replay from it must get to.
type loadedBase struct {
	snap  *snapshot
	reach reach
}

// reach is the newest listed checkpoint, which proves the log got that far;
// skipped is why the ones after the loaded one did not load. Zero is none.
type reach struct {
	sequence int64
	skipped  error
}

// reached checks a replay came as far as the reach.
func (r reach) reached(tip *snapshot) error {
	if tip.Sequence >= r.sequence {
		return nil
	}
	return fmt.Errorf("%w: the log ends at %d, before checkpoint %d: %w", blob.ErrIntegrity, tip.Sequence, r.sequence, r.skipped)
}

// loadBase reads the world marker and lists every checkpoint, then loads as
// newest does.
func (loader checkpointLoader) loadBase(ctx context.Context) (loadedBase, error) {
	markerCtx, cancel := loader.bound(ctx)
	err := readMarker(markerCtx, loader.objects, loader.worldID)
	cancel()
	if err != nil {
		return loadedBase{}, err
	}
	listCtx, cancel := loader.bound(ctx)
	sequences, err := listCheckpoints(listCtx, loader.objects, 0)
	cancel()
	if err != nil {
		return loadedBase{}, err
	}
	return loader.newest(ctx, sequences)
}

// newest loads as loadable does from sequences, every checkpoint listed; with
// none loading, genesis stands in.
func (loader checkpointLoader) newest(ctx context.Context, sequences []int64) (loadedBase, error) {
	base, err := loader.loadable(ctx, sequences)
	if err != nil || base.snap != nil {
		return base, err
	}
	failures := base.reach.skipped
	if len(sequences) == 0 {
		failures = errors.New("world has a marker but no checkpoint")
	}
	base.reach.skipped = fmt.Errorf("%w: no checkpoint loads: %w", blob.ErrIntegrity, failures)
	if err := loader.holdsSlotAfter(ctx, genesisSequence, base.reach.skipped); err != nil {
		return loadedBase{}, err
	}
	loader.logger.Warn("no checkpoint loads; replaying the whole log from genesis", "world", loader.worldID, "error", base.reach.skipped)
	loaded, err := genesisSnapshot()
	if err != nil {
		return loadedBase{}, err
	}
	base.snap = loaded
	return base, nil
}

// loadable loads the newest of sequences that loads, skipping one missing or
// corrupt in any object it names; snap is nil when none does. A bucket or
// caller failure is returned as it is: only damage costs replay.
func (loader checkpointLoader) loadable(ctx context.Context, sequences []int64) (loadedBase, error) {
	base := loadedBase{reach: reach{sequence: lastOr(sequences, genesisSequence)}}
	var failures []error
	for _, sequence := range slices.Backward(sequences) {
		loaded, err := loader.load(ctx, sequence)
		if err == nil {
			base.snap = loaded
			break
		}
		if !damaged(ctx, err) {
			return loadedBase{}, err
		}
		loader.logger.Warn("checkpoint does not load; trying the one before it", "world", loader.worldID, "checkpoint", sequence, "error", err)
		failures = append(failures, err)
	}
	base.reach.skipped = errors.Join(failures...)
	return base, nil
}

// damaged reports a checkpoint failing on its own objects, not on the bucket
// or the caller: the one reason to skip it.
func damaged(ctx context.Context, err error) bool {
	return ctx.Err() == nil && (errors.Is(err, blob.ErrIntegrity) || errors.Is(err, blob.ErrNotFound))
}

// load bounds one checkpoint's reads apart, so each fallback gets the
// loader's whole timeout.
func (loader checkpointLoader) load(ctx context.Context, sequence int64) (*snapshot, error) {
	ctx, cancel := loader.bound(ctx)
	defer cancel()
	checkpoint, err := readCheckpoint(ctx, loader.objects, loader.worldID, sequence)
	if err != nil {
		return nil, err
	}
	loaded, err := loadCheckpoint(ctx, loader.objects, checkpoint, loader.workers)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint %d: %w", sequence, err)
	}
	return loaded, nil
}

// holdsSlotAfter checks the log still holds the slot after sequence, so a
// replay from there misses none; cause when it was collected.
func (loader checkpointLoader) holdsSlotAfter(ctx context.Context, sequence int64, cause error) error {
	headCtx, cancel := loader.bound(ctx)
	defer cancel()
	_, err := loader.objects.Head(headCtx, slotKey(sequence+1))
	if errors.Is(err, blob.ErrNotFound) {
		return cause
	}
	if err != nil {
		return fmt.Errorf("head slot %d: %w", sequence+1, err)
	}
	return nil
}

// genesisSnapshot is the empty world checkpoint zero holds, on its layout.
func genesisSnapshot() (*snapshot, error) {
	_, ref, err := foldShard(0, 0, make([]foldedEntry, 0))
	if err != nil {
		return nil, fmt.Errorf("build genesis: %w", err)
	}
	loaded := newSnapshot()
	loaded.Sequence = genesisSequence
	loaded.Checkpoint = &checkpointBase{Sequence: genesisSequence, Shards: []shardRef{ref}}
	return loaded, nil
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

// listCheckpoints lists the sequences of the checkpoints after a known one,
// or of all from 0, ascending.
func listCheckpoints(ctx context.Context, objects blob.Store, after int64) ([]int64, error) {
	var sequences []int64
	for page, err := range sequencedPages(ctx, objects, checkpointPrefix, after) {
		if err != nil {
			return nil, err
		}
		for index := range page {
			sequences = append(sequences, page[index].sequence)
		}
	}
	return sequences, nil
}

// lastOr is s's last element, or fallback when s is empty.
func lastOr[T any](s []T, fallback T) T {
	if len(s) == 0 {
		return fallback
	}
	return s[len(s)-1]
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
// applied slot. A positive until is the last sequence applied; stop, asked
// after each slot, ends the replay early; a replay short of reach fails.
type replayOptions struct {
	worldID string
	workers int
	onSlot  func(slot *slotObject)
	until   int64
	stop    func(slot *slotObject) bool
	reach   reach
}

// replayChunk is how many slots a replay reads ahead of applying them.
const replayChunk = 256

// replay applies every slot after loaded's sequence, a page of names at a
// time, reading the slots in parallel a chunk at a time.
func replay(ctx context.Context, objects blob.Store, loaded *snapshot, options replayOptions) error {
	if err := applyLog(ctx, objects, loaded, options); err != nil {
		return err
	}
	return options.reach.reached(loaded)
}

func applyLog(ctx context.Context, objects blob.Store, loaded *snapshot, options replayOptions) error {
	for page, err := range sequencedPages(ctx, objects, logPrefix, loaded.Sequence) {
		if err != nil {
			return err
		}
		listed := len(page)
		if options.until > 0 {
			page = slices.DeleteFunc(page, func(slot sequenced) bool { return slot.sequence > options.until })
		}
		for chunk := range slices.Chunk(page, replayChunk) {
			slots, err := readSlots(ctx, objects, options, chunk)
			if err != nil {
				return err
			}
			for _, read := range slots {
				if err := loaded.applySlot(read); err != nil {
					return &applyError{fmt.Errorf("replay: %w", err)}
				}
				if options.onSlot != nil {
					options.onSlot(read.slot)
				}
				if options.stop != nil && options.stop(read.slot) {
					return nil
				}
			}
		}
		if len(page) < listed {
			return nil
		}
	}
	return nil
}

// readSlots reads the listed slots in parallel; a listed slot that is
// missing is an integrity failure.
func readSlots(ctx context.Context, objects blob.Store, options replayOptions, listed []sequenced) ([]slotRead, error) {
	slots := make([]slotRead, len(listed))
	err := runParallel(ctx, options.workers, indexes(len(listed)), func(ctx context.Context, index int) error {
		read, err := readSlot(ctx, objects, options.worldID, listed[index].sequence)
		if errors.Is(err, blob.ErrNotFound) {
			return fmt.Errorf("%w: listed slot %d is missing: %w", blob.ErrIntegrity, listed[index].sequence, err)
		}
		slots[index] = read
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	return slots, nil
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
	object, err := getVerified(ctx, objects, keyed)
	if err != nil {
		var zero T
		return zero, err
	}
	return decodeChecked(keyed.Key, object.Data, validate)
}

// getVerified reads a referenced object and checks its bytes hash to it.
func getVerified(ctx context.Context, objects objectGetter, keyed keyedRef) (blob.Object, error) {
	object, err := getReferenced(ctx, objects, keyed)
	if err == nil {
		err = verifyBlobHash(keyed.objectRef, object.Data)
	}
	if err != nil {
		return blob.Object{}, err
	}
	return object, nil
}

// getReferenced is getVerified without the hash check, for a caller that
// verifies the bytes another way: a missing object is an integrity failure
// that also matches blob.ErrNotFound.
func getReferenced(ctx context.Context, objects objectGetter, keyed keyedRef) (blob.Object, error) {
	ref := keyed.objectRef
	if err := verifyRef(ref, keyed.expectedKey); err != nil {
		return blob.Object{}, fmt.Errorf("%w: %v", blob.ErrIntegrity, err)
	}
	object, err := objects.Get(ctx, ref.Key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return blob.Object{}, fmt.Errorf("%w: referenced object %q is missing: %w", blob.ErrIntegrity, ref.Key, err)
		}
		return blob.Object{}, fmt.Errorf("read referenced object %q: %w", ref.Key, err)
	}
	if err := validateReadObject(ref.Key, &object); err != nil {
		return blob.Object{}, err
	}
	return object, nil
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
