package bucketstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

const (
	// A store checkpoints once the newest checkpoint lags its snapshot by
	// checkpointSlots slots (a replay budget: spread writes rewrite every
	// shard, ADR 0036), or by checkpointAge since the first slot after it.
	checkpointSlots  = 8192
	checkpointAge    = 10 * time.Minute
	checkpointJitter = 10 * time.Second
	// checkpointTimeout bounds one compaction step or adoption, which no
	// request waits for.
	checkpointTimeout = 5 * time.Minute
	// checkpointStep bounds the documents one checkpoint folds: a backlog past
	// it is checkpointed at earlier slot boundaries first, so every step ends
	// within checkpointTimeout and progress accumulates.
	checkpointStep = 1 << 15
	// The newest checkpointsKept stay, and any whose successor came within
	// the store's grace. A dropped one takes the shards, segments and root
	// only it used, each past the grace and at the generation read.
	checkpointsKept = 3
	// defaultCheckpointGrace is the grace when Options leaves it zero.
	defaultCheckpointGrace = minCheckpointGrace
	// minCheckpointGrace leaves a compactor that reused bytes just younger
	// than freshenAge time to finish (checkpointTimeout) before they can go.
	minCheckpointGrace = 2 * freshenAge
	// A slot the oldest kept checkpoint covers goes once slotRetention old,
	// for resumes and the federation deriver's cursors. A replica unconfirmed
	// for staleAfter reloads before trusting a missing slot as the tip.
	slotRetention = 24 * time.Hour
	staleAfter    = slotRetention / 2
)

// A drop must not take bytes a compactor reused just under freshenAge ago
// and is still writing, so the build fails if the grace cannot cover it.
const _ = uint64(minCheckpointGrace - freshenAge - checkpointTimeout)

// compactionTrigger is when a store's compactor runs; tests shorten it.
type compactionTrigger struct {
	slots int
	age   time.Duration
	wait  func() time.Duration
	// timeout and step override checkpointTimeout and checkpointStep when set.
	timeout time.Duration
	step    int
}

var defaultTrigger = compactionTrigger{
	slots: checkpointSlots,
	age:   checkpointAge,
	wait:  func() time.Duration { return jitterWithin(checkpointJitter) },
}

// compaction schedules a store's compactor: one run at a time, started by
// installed slots, stopped with the store.
type compaction struct {
	trigger compactionTrigger
	ctx     context.Context
	cancel  context.CancelFunc
	// base is the snapshot at the newest checkpoint, rebased on it, which a
	// bounded step replays from; it shares every unchanged node with the
	// served snapshot. Nil until a load or checkpoint gives one.
	base atomic.Pointer[snapshot]
	// written is the newest checkpoint this store wrote itself.
	written atomic.Int64

	mu      sync.Mutex
	slots   int       // installed since the newest checkpoint this store knows
	since   time.Time // when the first of them was installed
	running bool
	closed  bool
	done    sync.WaitGroup
}

func (c *compaction) timeout() time.Duration { return cmp.Or(c.trigger.timeout, checkpointTimeout) }

func (c *compaction) stepLimit() int { return cmp.Or(c.trigger.step, checkpointStep) }

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

// compact runs the compactor after its wait, a step at a time until the
// served snapshot is checkpointed or a step fails, and forgets the slots it
// covered: those counted when it started.
func (store *Store) compact(counted int) {
	c := &store.compaction
	err := waitForRetry(c.ctx, c.trigger.wait())
	for more := err == nil; more; {
		ctx, cancel := context.WithTimeout(c.ctx, c.timeout())
		more, err = store.step(ctx)
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

// checkpoint brings the store's newest checkpoint up to its snapshot, in as
// many steps as it takes.
func (store *Store) checkpoint(ctx context.Context) error {
	for {
		more, err := store.step(ctx)
		if err != nil || !more {
			return err
		}
	}
}

// step adopts a peer's newer checkpoint, ending the run since that peer is
// compacting, or writes one of the served snapshot, or of an earlier slot
// boundary when its changes exceed a step; more while served still lags.
func (store *Store) step(ctx context.Context) (bool, error) {
	// Only checkpoints after the known one are listed: the rest may be many
	// until they are collected.
	known := store.layout().Sequence
	newest, err := newestCheckpointSequence(ctx, store.objects, known)
	if err != nil {
		return false, err
	}
	if newest > known {
		return false, store.adoptPeerCheckpoint(ctx, newest)
	}
	snap := store.served.Load().snap
	if store.readOnly || snap.Sequence <= newest {
		return false, nil
	}
	// A snapshot that failed to rebase on the newest checkpoint would name
	// shards of an older one, which a drop may already have taken.
	if snap.Checkpoint.Sequence != known {
		return false, fmt.Errorf("%w: snapshot rests on checkpoint %d, not the newest %d", blob.ErrIntegrity, snap.Checkpoint.Sequence, known)
	}
	if deferred, err := store.deferToPeer(ctx, known); deferred || err != nil {
		return false, err
	}
	target, err := store.stepTarget(ctx, snap)
	if err != nil {
		return false, err
	}
	adopted, err := store.checkpointWriter().write(ctx, target)
	if err != nil {
		return false, err
	}
	store.installAdoption(adopted)
	if adopted.own {
		store.compaction.written.Store(target.Sequence)
	}
	store.keepBase(target, adopted)
	store.logger.Info("checkpoint written", "world", store.worldID, "sequence", target.Sequence, "served", snap.Sequence)
	plan, err := store.planDrop(ctx)
	if err == nil {
		err = errors.Join(store.applyDrop(ctx, plan), store.dropSlots(ctx, plan))
	}
	if err != nil {
		store.logger.Warn("old checkpoints or slots not dropped; a later checkpoint retries", "world", store.worldID, "error", err)
	}
	return target != snap, nil
}

// deferToPeer reports whether the newest checkpoint is a peer's written
// within one step bound: that peer is compacting, and one that died is
// replaced after the bound. Checkpoint zero and own checkpoints never defer.
func (store *Store) deferToPeer(ctx context.Context, known int64) (bool, error) {
	c := &store.compaction
	if known <= 1 || known == c.written.Load() {
		return false, nil
	}
	attributes, err := store.objects.Head(ctx, checkpointKey(known))
	if err != nil {
		return false, fmt.Errorf("head checkpoint %d: %w", known, err)
	}
	return store.now().Sub(attributes.Modified) < c.timeout(), nil
}

// stepTarget is the snapshot the next checkpoint covers: snap when its
// changes fit one step, else the base replayed to the slot boundary that
// fills a step; without a base at the newest checkpoint, the whole backlog.
func (store *Store) stepTarget(ctx context.Context, snap *snapshot) (*snapshot, error) {
	c := &store.compaction
	limit := c.stepLimit()
	if changedDocuments(snap, limit) <= limit {
		return snap, nil
	}
	base := c.base.Load()
	if base == nil || base.Checkpoint.Sequence != snap.Checkpoint.Sequence {
		store.logger.Warn("no snapshot at the newest checkpoint; checkpointing the whole backlog", "world", store.worldID, "checkpoint", snap.Checkpoint.Sequence)
		return snap, nil
	}
	target := store.derive(base)
	touched := make(map[string]struct{}, limit)
	options := replayOptions{
		worldID: store.worldID, workers: store.shardWorkers, until: snap.Sequence,
		stop: func(slot *slotObject) bool {
			for index := range slot.Entries {
				touched[slot.Entries[index].Path] = struct{}{}
			}
			return len(touched) >= limit
		},
	}
	if err := replay(ctx, store.objects, target, options); err != nil {
		return nil, fmt.Errorf("checkpoint step from %d: %w", base.Sequence, err)
	}
	if target.Sequence >= snap.Sequence {
		return snap, nil
	}
	return target, nil
}

// changedDocuments counts the documents changed since snap's checkpoint, up
// to limit+1.
func changedDocuments(snap *snapshot, limit int) int {
	count := 0
	snap.Paths.Ascend(func(state *pathState) bool {
		if !state.unchanged() {
			count++
		}
		return count <= limit
	})
	return count
}

// keepBase keeps target, rebased on the checkpoint just written of it, as
// the base the next step replays from.
func (store *Store) keepBase(target *snapshot, adopted *adoption) {
	next := store.derive(target)
	if err := next.adopt(adopted); err != nil {
		store.logger.Error("snapshot at the new checkpoint failed to rebase; the next step writes the whole backlog", "world", store.worldID, "checkpoint", adopted.checkpoint.Sequence, "error", err)
		store.compaction.base.Store(nil)
		return
	}
	store.compaction.base.Store(next)
}

// advanceBase replays the base to a peer's checkpoint and rebases it there,
// so this store can step from it too. A base that cannot follow is dropped;
// the next checkpoint this store writes sets one again.
func (store *Store) advanceBase(ctx context.Context, from int64, a *adoption) {
	c := &store.compaction
	base := c.base.Load()
	if store.readOnly || base == nil {
		return
	}
	if base.Checkpoint.Sequence != from {
		c.base.Store(nil)
		return
	}
	next := store.derive(base)
	err := replay(ctx, store.objects, next, replayOptions{worldID: store.worldID, workers: store.shardWorkers, until: a.checkpoint.Sequence})
	if err == nil && next.Sequence != a.checkpoint.Sequence {
		err = fmt.Errorf("%w: checkpoint %d is not a slot boundary; replay reached %d", blob.ErrIntegrity, a.checkpoint.Sequence, next.Sequence)
	}
	if err == nil {
		err = next.adopt(a)
	}
	if err != nil {
		store.logger.Warn("snapshot at the peer's checkpoint not built; the next own checkpoint sets one", "world", store.worldID, "checkpoint", a.checkpoint.Sequence, "error", err)
		c.base.Store(nil)
		return
	}
	c.base.Store(next)
}

// grouped is a document with its path hash, in the shard the hash names.
type grouped struct {
	state *pathState
	hash  string
}

// checkpointWriter writes one world's checkpoints, workers objects at a time.
type checkpointWriter struct {
	bodyReader
	workers int
	// now dates the segment window; grace fences the sweep of old segments;
	// pathHash hashes the paths of the shards written.
	now      func() time.Time
	grace    time.Duration
	pathHash func(path string) string
}

func (store *Store) checkpointWriter() checkpointWriter {
	return checkpointWriter{bodyReader: store.bodyReader(), workers: store.shardWorkers, now: store.now, grace: store.checkpointGrace, pathHash: pathHash}
}

// write writes snap's changed history blocks and shards, the root, the
// checkpoint, then the shards' segments, which the root does not name: a
// missing one costs readers blob reads and never fails the checkpoint.
func (writer checkpointWriter) write(ctx context.Context, snap *snapshot) (*adoption, error) {
	objects, worldID, workers := writer.objects, writer.worldID, writer.workers
	bits := shardBitsFor(snap.Paths.Len())
	byShard := make([][]*pathState, 1<<bits)
	snap.Paths.Ascend(func(state *pathState) bool {
		index := state.shard(bits)
		byShard[index] = append(byShard[index], state)
		return true
	})
	previous := snap.Checkpoint
	reuse := previous.Bits == bits
	// Only a shard that is written needs its documents' path hashes.
	groups := make([][]grouped, 1<<bits)
	var shards []int
	var changed []grouped
	for index, states := range byShard {
		if reuse && !slices.ContainsFunc(states, func(state *pathState) bool { return !state.unchanged() }) {
			continue
		}
		shards = append(shards, index)
		groups[index] = make([]grouped, len(states))
		for position, state := range states {
			document := grouped{state: state, hash: writer.pathHash(state.Path)}
			groups[index][position] = document
			if !state.unchanged() {
				changed = append(changed, document)
			}
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
	own := false
	if err == nil {
		own, err = createImmutableOwn(ctx, objects, modelObject{Key: checkpointKey(snap.Sequence), Data: data})
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint %d: %w", snap.Sequence, err)
	}
	now := writer.now()
	run := &segmentRun{
		reader: writer.bodyReader, workers: workers, window: windowOf(now), bits: bits, previousBits: previous.Bits,
		fence: dropFence{started: now, grace: writer.grace},
	}
	run.write(ctx, groups, shards)
	return &adoption{checkpoint: &checkpointBase{Sequence: snap.Sequence, Bits: bits, Shards: refs}, entries: entries, own: own}, nil
}

// foldHistory is a document's history blocks, one per absolute block of
// retained versions; it creates the blocks its checkpoint entry lacks.
func foldHistory(ctx context.Context, objects blob.Store, document grouped) ([]blockRef, error) {
	state := document.state
	var base []blockRef
	baseCurrent := 0
	if state.Base != nil {
		base, baseCurrent = state.Base.History, state.Base.Current
	}
	first := state.First
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
		history := historyObject{Schema: historySchema, PathHash: document.hash, First: from, Last: to, Entries: versions}
		if err := validateHistoryObject(&history); err != nil {
			return nil, fmt.Errorf("%w: versions %d-%d: %v", blob.ErrIntegrity, from, to, err)
		}
		object, ref, err := immutableJSON(historyKey, history)
		if err == nil {
			err = createContent(ctx, objects, object)
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
