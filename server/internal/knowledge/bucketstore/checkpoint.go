package bucketstore

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

const (
	// A store checkpoints once the newest checkpoint lags its snapshot by
	// checkpointSlots slots, or by checkpointAge since the first slot after
	// it, after a random wait so racing replicas usually find one written.
	checkpointSlots  = 1024
	checkpointAge    = 60 * time.Second
	checkpointJitter = 10 * time.Second
	// checkpointTimeout bounds one compaction or adoption, which no request
	// waits for.
	checkpointTimeout = 5 * time.Minute
)

// compactionTrigger is when a store's compactor runs; tests shorten it.
type compactionTrigger struct {
	slots int
	age   time.Duration
	wait  func() time.Duration
}

var defaultTrigger = compactionTrigger{
	slots: checkpointSlots,
	age:   checkpointAge,
	wait:  func() time.Duration { return jitterWithin(checkpointJitter, mathrand.Int64N) },
}

// compaction schedules a store's compactor: one run at a time, started by
// installed slots, stopped with the store.
type compaction struct {
	trigger compactionTrigger
	ctx     context.Context
	cancel  context.CancelFunc

	mu      sync.Mutex
	slots   int       // installed since the newest checkpoint this store knows
	since   time.Time // when the first of them was installed
	running bool
	closed  bool
	done    sync.WaitGroup
}

// noteSlots counts installed slots and starts the compactor once they lag
// enough. A world gone quiet keeps its last few slots until the next write.
func (store *Store) noteSlots(n int) {
	c := &store.compaction
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || n == 0 {
		return
	}
	if c.slots == 0 {
		c.since = store.now()
	}
	c.slots += n
	if c.running || c.slots < c.trigger.slots && store.now().Sub(c.since) < c.trigger.age {
		return
	}
	c.running = true
	c.done.Add(1)
	go store.compact(c.slots)
}

// compact runs the compactor once, after its wait, and forgets the slots it
// covered: those counted when it started.
func (store *Store) compact(counted int) {
	c := &store.compaction
	err := waitForRetry(c.ctx, c.trigger.wait())
	if err == nil {
		ctx, cancel := context.WithTimeout(c.ctx, checkpointTimeout)
		err = store.checkpoint(ctx)
		cancel()
	}
	if err != nil && c.ctx.Err() == nil {
		store.logger.Warn("checkpoint failed; replay grows until one succeeds", "world", store.worldID, "error", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	c.slots = max(c.slots-counted, 0)
	c.since = store.now()
	c.done.Done()
}

// stopCompaction ends a run in progress and refuses new ones.
func (store *Store) stopCompaction() {
	c := &store.compaction
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.done.Wait()
}

// checkpoint brings the store's newest checkpoint up to its snapshot: it
// adopts one a peer wrote meanwhile, or writes one unless read-only.
func (store *Store) checkpoint(ctx context.Context) error {
	newest, err := newestCheckpointSequence(ctx, store.objects)
	if err != nil {
		return err
	}
	if newest > store.layout().Sequence {
		return store.adoptPeerCheckpoint(ctx, newest)
	}
	snap := store.served.Load().snap
	if store.readOnly || snap.Sequence <= newest {
		return nil
	}
	adopted, err := store.checkpointWriter().write(ctx, snap)
	if err != nil {
		return err
	}
	store.installAdoption(adopted)
	return nil
}

// layout is the checkpoint the store last rebased on.
func (store *Store) layout() *checkpointBase {
	if adopted := store.adoption.Load(); adopted != nil {
		return adopted.checkpoint
	}
	return store.served.Load().snap.Checkpoint
}

// adoptPeerCheckpoint rebases on a checkpoint another store wrote, reading
// only the shards that differ from the one this store rests on.
func (store *Store) adoptPeerCheckpoint(ctx context.Context, sequence int64) error {
	checkpoint, _, err := getValidated(ctx, store.objects, checkpointKey(sequence), func(checkpoint *checkpointObject) error {
		return validateCheckpoint(checkpoint, sequence)
	})
	if err != nil {
		return fmt.Errorf("adopt checkpoint %d: %w", sequence, err)
	}
	if checkpoint.WorldID != store.worldID {
		return fmt.Errorf("%w: checkpoint %d belongs to world %q", blob.ErrIntegrity, sequence, checkpoint.WorldID)
	}
	root, err := loadRoot(ctx, store.objects, checkpoint)
	if err != nil {
		return fmt.Errorf("adopt checkpoint %d: %w", sequence, err)
	}
	current := store.layout()
	var changed []int
	for index, ref := range root.layout.Shards {
		if current.Legacy || root.layout.Legacy || current.Bits != root.layout.Bits || current.Shards[index] != ref {
			changed = append(changed, index)
		}
	}
	shards, err := shardReader{objects: store.objects, layout: root.layout, workers: store.shardWorkers}.states(ctx, changed)
	if err != nil {
		return fmt.Errorf("adopt checkpoint %d: %w", sequence, err)
	}
	// Entries the served snapshot rests on already are left out, so the
	// adoption holds only what changed.
	served := store.served.Load().snap
	entries := make(map[string]*baseEntry)
	for _, states := range shards {
		for _, state := range states {
			if old := served.path(state.Path); old == nil || !sameBase(old.Base, state.Base) {
				entries[state.Path] = state.Base
			}
		}
	}
	store.installAdoption(&adoption{checkpoint: root.layout, entries: entries})
	return nil
}

// adoption is a checkpoint the store's snapshots rebase on: its layout and
// the entry of every document that changed in it.
type adoption struct {
	checkpoint *checkpointBase
	entries    map[string]*baseEntry
}

// installAdoption makes a the newest checkpoint and rebases the served
// snapshot on it; snapshots built earlier rebase as they are installed.
func (store *Store) installAdoption(a *adoption) {
	store.refreshMu.Lock()
	defer store.refreshMu.Unlock()
	if current := store.adoption.Load(); current != nil && current.checkpoint.Sequence >= a.checkpoint.Sequence {
		return
	}
	store.adoption.Store(a)
	current := store.served.Load()
	store.served.Store(&served{snap: store.rebased(current.snap), confirmed: current.confirmed})
}

// rebased is s on the newest adopted checkpoint when s was built before it
// and holds it; otherwise s.
func (store *Store) rebased(s *snapshot) *snapshot {
	a := store.adoption.Load()
	if a == nil || s.Checkpoint.Sequence >= a.checkpoint.Sequence || s.Sequence < a.checkpoint.Sequence {
		return s
	}
	next := store.derive(s)
	if err := next.adopt(a); err != nil {
		store.logger.Error("rebase on checkpoint failed", "world", store.worldID, "checkpoint", a.checkpoint.Sequence, "error", err)
		return s
	}
	return next
}

// adopt rebases a derived snapshot at or past a's checkpoint on it, dropping
// the versions the checkpoint now holds.
func (s *snapshot) adopt(a *adoption) error {
	for path, base := range a.entries {
		old := s.path(path)
		if old == nil || old.Current < base.Current {
			return fmt.Errorf("%w: checkpoint %d holds %s ahead of the log", blob.ErrIntegrity, a.checkpoint.Sequence, path)
		}
		if sameBase(old.Base, base) {
			continue
		}
		state := *old
		state.Base, state.Recent = base, nil
		if index := slices.IndexFunc(old.Recent, func(version retainedVersion) bool { return version.entry.Version > base.Current }); index >= 0 {
			// Cloned: the old array holds the versions the checkpoint took.
			state.Recent = slices.Clone(old.Recent[index:])
		}
		if state.First == 0 {
			state.First = base.first()
		}
		s.put(old, &state)
	}
	s.Checkpoint = a.checkpoint
	return nil
}

func sameBase(a, b *baseEntry) bool {
	return a != nil && b != nil && slices.Equal(a.History, b.History) && a.Manifest == b.Manifest &&
		a.Current == b.Current && a.Archived == b.Archived && a.BodyHash == b.BodyHash && a.Modified.Equal(b.Modified)
}

// grouped is a document with its path hash, in the shard the hash names.
type grouped struct {
	state *pathState
	hash  string
}

// checkpointWriter writes one world's checkpoints, workers objects at a time.
type checkpointWriter struct {
	objects blob.Store
	worldID string
	workers int
}

func (store *Store) checkpointWriter() checkpointWriter {
	return checkpointWriter{objects: store.objects, worldID: store.worldID, workers: store.shardWorkers}
}

// write writes snap's checkpoint: changed documents' history blocks, changed
// shards, the root, then the checkpoint. Its bytes follow from the log alone,
// so compactors racing on one sequence write the same objects.
func (writer checkpointWriter) write(ctx context.Context, snap *snapshot) (*adoption, error) {
	objects, worldID, workers := writer.objects, writer.worldID, writer.workers
	bits := shardBitsFor(snap.Paths.Len())
	groups := make([][]grouped, 1<<bits)
	snap.Paths.Ascend(func(state *pathState) bool {
		hash := pathHash(state.Path)
		index := shardOf(hash, bits)
		groups[index] = append(groups[index], grouped{state: state, hash: hash})
		return true
	})
	previous := snap.Checkpoint
	reuse := !previous.Legacy && previous.Bits == bits
	var shards []int
	var changed []grouped
	for index, group := range groups {
		dirty := !reuse
		for _, document := range group {
			if !document.state.unchanged() {
				dirty = true
				changed = append(changed, document)
			}
		}
		if dirty {
			shards = append(shards, index)
		}
	}

	var mu sync.Mutex
	entries := make(map[string]*baseEntry, len(changed))
	err := runParallel(ctx, workers, changed, func(ctx context.Context, document grouped) error {
		blocks, err := foldHistory(ctx, objects, document)
		if err != nil {
			return fmt.Errorf("fold history of %s: %w", document.state.Path, err)
		}
		state := document.state
		base := &baseEntry{History: blocks, Current: state.Current, Archived: state.Archived, BodyHash: state.BodyHash, Modified: state.Modified}
		mu.Lock()
		entries[state.Path] = base
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("checkpoint %d: %w", snap.Sequence, err)
	}

	refs := make([]shardRef, 1<<bits)
	if reuse {
		copy(refs, previous.Shards)
	}
	err = runParallel(ctx, workers, shards, func(ctx context.Context, index int) error {
		folded := make([]foldedEntry, len(groups[index]))
		for position, document := range groups[index] {
			base := entries[document.state.Path]
			if base == nil {
				base = document.state.Base
			}
			folded[position] = foldedEntryOf(document, base)
		}
		object, ref, err := foldShard(index, bits, folded)
		if err == nil {
			err = createImmutable(ctx, objects, object)
		}
		refs[index] = ref
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("checkpoint %d: %w", snap.Sequence, err)
	}
	root, rootRef, err := foldRoot(worldID, snap.Paths.Len(), bits, refs)
	if err == nil {
		err = createImmutable(ctx, objects, root)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint %d: %w", snap.Sequence, err)
	}
	checkpoint := checkpointObject{Schema: logSchema, WorldID: worldID, Sequence: snap.Sequence, Root: rootRef, Tip: snap.Tip}
	data, err := marshalImmutable(checkpoint)
	if err == nil {
		err = createImmutable(ctx, objects, modelObject{Key: checkpointKey(snap.Sequence), Data: data})
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint %d: %w", snap.Sequence, err)
	}
	return &adoption{checkpoint: &checkpointBase{Sequence: snap.Sequence, Bits: bits, Shards: refs}, entries: entries}, nil
}

// foldHistory is a document's history blocks, one per absolute block of
// retained versions; it creates the blocks its checkpoint entry lacks.
func foldHistory(ctx context.Context, objects blob.Store, document grouped) ([]blockRef, error) {
	state := document.state
	var base []blockRef
	baseCurrent := 0
	if state.Base != nil {
		var err error
		if base, err = baseBlocks(ctx, objects, state); err != nil {
			return nil, err
		}
		baseCurrent = state.Base.Current
	}
	first := state.First
	if first == 0 && len(base) > 0 {
		// A schema 1 entry nothing changed since.
		first = base[0].First
	}
	if first < 1 {
		return nil, fmt.Errorf("%w: no first retained version", blob.ErrIntegrity)
	}
	var blocks []blockRef
	for block := (first - 1) / historyBlockSize; block <= (state.Current-1)/historyBlockSize; block++ {
		from, to := max(first, block*historyBlockSize+1), min(state.Current, (block+1)*historyBlockSize)
		previous, found := blockAt(base, block)
		if found && previous.First == from && previous.Last == to {
			blocks = append(blocks, previous)
			continue
		}
		var versions []historyEntry
		if found && previous.Last >= from {
			history, err := readBlock(ctx, objects, document.hash, previous)
			if err != nil {
				return nil, err
			}
			for _, entry := range history.Entries {
				if entry.Version >= from {
					versions = append(versions, entry)
				}
			}
		}
		for _, version := range state.Recent {
			if version.entry.Version > baseCurrent && version.entry.Version >= from && version.entry.Version <= to {
				versions = append(versions, version.entry)
			}
		}
		history := historyObject{Schema: schemaVersion, PathHash: document.hash, First: from, Last: to, Entries: versions}
		if err := validateHistoryObject(&history); err != nil {
			return nil, fmt.Errorf("%w: versions %d-%d: %v", blob.ErrIntegrity, from, to, err)
		}
		object, ref, err := immutableJSON(historyKey, history)
		if err == nil {
			err = createImmutable(ctx, objects, object)
		}
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, blockRef{First: from, Last: to, Hash: ref.Hash})
	}
	return blocks, nil
}

// blockAt is the block of blocks that covers absolute block index.
func blockAt(blocks []blockRef, index int) (blockRef, bool) {
	for _, ref := range blocks {
		if (ref.First-1)/historyBlockSize == index {
			return ref, true
		}
	}
	return blockRef{}, false
}

func foldedEntryOf(document grouped, base *baseEntry) foldedEntry {
	state := document.state
	return foldedEntry{
		Path: state.Path, PathHash: document.hash, Current: state.Current, Archived: state.Archived,
		BodyHash: state.BodyHash, Modified: state.Modified.Format(time.RFC3339),
		Catalog: catalogRecordOf(state.Entry), History: base.History,
	}
}

// foldShard is one folded shard's object and the root's reference to it.
func foldShard(index, bits int, entries []foldedEntry) (modelObject, shardRef, error) {
	label := shardLabel(index, bits)
	shard := foldedShard{Schema: foldedSchema, ShardBits: bits, Shard: label, Entries: entries}
	if err := validateFoldedShard(&shard, index, bits); err != nil {
		return modelObject{}, shardRef{}, fmt.Errorf("build shard %s: %w", label, err)
	}
	object, ref, err := immutableJSON(func(hash string) string { return shardKey(label, hash) }, shard)
	if err != nil {
		return modelObject{}, shardRef{}, fmt.Errorf("build shard %s: %w", label, err)
	}
	return object, shardRef{Shard: label, objectRef: ref}, nil
}

func foldRoot(worldID string, documents, bits int, shards []shardRef) (modelObject, objectRef, error) {
	root := foldedRoot{Schema: foldedSchema, WorldID: worldID, DocumentCount: documents, ShardBits: bits, Shards: shards}
	if err := validateFoldedRoot(&root, worldID); err != nil {
		return modelObject{}, objectRef{}, fmt.Errorf("build root: %w", err)
	}
	return immutableJSON(rootKey, root)
}
