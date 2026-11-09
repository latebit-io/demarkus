package bucketstore

import (
	"cmp"
	"context"
	"encoding/json"
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
	// backstopDelay is how old the newest checkpoint must be before a
	// backstop steps: an eager replica's trigger, its step, and one more.
	backstopDelay = checkpointAge + 2*checkpointTimeout
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
	// timeout, step and delay override checkpointTimeout, checkpointStep and
	// backstopDelay when set.
	timeout time.Duration
	step    int
	delay   time.Duration
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
	role    CompactionRole
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

func (c *compaction) backstopDelay() time.Duration { return cmp.Or(c.trigger.delay, backstopDelay) }

// keepsBase reports whether this store keeps a snapshot at the newest
// checkpoint to step from: an eager writer does, a backstop or a read-only
// store never steps from one.
func (store *Store) keepsBase() bool {
	return !store.readOnly && store.compaction.role == Eager
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
	newer, err := listCheckpoints(ctx, store.objects, known)
	if err != nil {
		return false, err
	}
	adopted, broken, err := store.adoptNewer(ctx, newer)
	if adopted || err != nil {
		return false, err
	}
	// A broken checkpoint still holds its name, so the next is written past it.
	newest := lastOr(newer, known)
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
	target, err := store.stepTarget(ctx, snap, broken)
	if err != nil {
		return false, err
	}
	written, err := store.checkpointWriter().write(ctx, target)
	if err != nil {
		return false, err
	}
	store.installAdoption(written)
	if written.own {
		store.compaction.written.Store(target.Sequence)
	}
	store.keepBase(target, written)
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

// adoptNewer adopts the newest of the newer checkpoints that loads, skipping
// one missing or corrupt in any object it names; it returns those skipped.
// A bucket or caller failure ends it, as no reason to skip.
func (store *Store) adoptNewer(ctx context.Context, newer []int64) (adopted bool, broken []int64, err error) {
	for _, sequence := range slices.Backward(newer) {
		if err = store.adoptPeerCheckpoint(ctx, sequence); err == nil {
			return true, broken, nil
		}
		if !damaged(ctx, err) {
			return false, broken, err
		}
		store.logger.Warn("newer checkpoint does not load; skipping it", "world", store.worldID, "checkpoint", sequence, "error", err)
		broken = append(broken, sequence)
	}
	return false, broken, nil
}

// deferToPeer reports whether the newest checkpoint is one to leave alone:
// for an eager store a peer's within one step bound (that peer is compacting;
// zero and its own never defer), for a backstop any one within its delay.
func (store *Store) deferToPeer(ctx context.Context, known int64) (bool, error) {
	c := &store.compaction
	hold := c.timeout()
	if c.role == Backstop {
		hold = c.backstopDelay()
	} else if known <= 1 || known == c.written.Load() {
		return false, nil
	}
	attributes, err := store.objects.Head(ctx, checkpointKey(known))
	if err != nil {
		return false, fmt.Errorf("head checkpoint %d: %w", known, err)
	}
	return store.now().Sub(attributes.Modified) < hold, nil
}

// stepTarget is the snapshot the next checkpoint covers: snap when its
// changes fit one step, else the base replayed to the slot boundary that
// fills a step, never a broken checkpoint's; without a base, the whole backlog.
func (store *Store) stepTarget(ctx context.Context, snap *snapshot, broken []int64) (*snapshot, error) {
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
			// Steps are deterministic, so ending where a broken one did would
			// meet its bytes under create-if-absent on every run.
			return len(touched) >= limit && !slices.Contains(broken, slot.last())
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
// the base the next step replays from; a backstop keeps none.
func (store *Store) keepBase(target *snapshot, adopted *adoption) {
	if !store.keepsBase() {
		return
	}
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
	if !store.keepsBase() || base == nil {
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
	// budget is the bytes a shard may hold before the layout widens.
	budget int
}

func (store *Store) checkpointWriter() checkpointWriter {
	return checkpointWriter{bodyReader: store.bodyReader(), workers: store.shardWorkers, now: store.now, grace: store.checkpointGrace, pathHash: pathHash, budget: store.shardBudget}
}

// write writes snap's changed history blocks and shards, the root, the
// checkpoint, then the shards' segments, which the root does not name: a
// missing one costs readers blob reads and never fails the checkpoint.
func (writer checkpointWriter) write(ctx context.Context, snap *snapshot) (*adoption, error) {
	objects, worldID, workers := writer.objects, writer.worldID, writer.workers
	previous := snap.Checkpoint
	// Bits never fall: a layout the count rule outgrew, or one a shard's size
	// forced, is kept, else every checkpoint would fold the world again.
	layout := writer.group(snap, max(shardBitsFor(snap.Paths.Len()), previous.Bits))
	// The changed set is the same at every width: a shard holding a change is
	// always written.
	entries, err := writer.foldChanged(ctx, layout.changed)
	if err == nil {
		layout, err = writer.fit(ctx, snap, layout, entries)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint %d: %w", snap.Sequence, err)
	}

	refs := make([]shardRef, 1<<layout.bits)
	if layout.reuse {
		copy(refs, previous.Shards)
	}
	err = runParallel(ctx, workers, layout.shards, func(ctx context.Context, index int) error {
		object, ref, err := layout.fold(index, entries)
		if err == nil {
			err = createImmutable(ctx, objects, object)
		}
		refs[index] = ref
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("checkpoint %d: %w", snap.Sequence, err)
	}
	root, rootRef, err := foldRoot(worldID, snap.Paths.Len(), layout.bits, refs)
	if err == nil && len(root.Data) > MaxObjectBytes {
		err = fmt.Errorf("root of %d shards is %d bytes, limit is %d", len(refs), len(root.Data), MaxObjectBytes)
	}
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
		reader: writer.bodyReader, workers: workers, window: windowOf(now), bits: layout.bits, previousBits: previous.Bits,
		fence: dropFence{started: now, grace: writer.grace},
	}
	run.write(ctx, layout.groups, layout.shards)
	return &adoption{checkpoint: &checkpointBase{Sequence: snap.Sequence, Bits: layout.bits, Shards: refs}, entries: entries, own: own}, nil
}

// shardLayout is a snapshot's documents at one shard width: the shards to
// write, their documents, and the changed documents among them.
type shardLayout struct {
	bits    int
	groups  [][]grouped
	shards  []int
	changed []grouped
	// reuse keeps the previous checkpoint's unchanged shards.
	reuse bool
}

// group lays snap's documents out at bits; with the previous width, an
// unchanged shard is reused: created, so under the object limit, and budgeted
// when a change next writes it. Only a written shard's paths are hashed.
func (writer checkpointWriter) group(snap *snapshot, bits int) shardLayout {
	byShard := make([][]*pathState, 1<<bits)
	snap.Paths.Ascend(func(state *pathState) bool {
		index := state.shard(bits)
		byShard[index] = append(byShard[index], state)
		return true
	})
	layout := shardLayout{bits: bits, groups: make([][]grouped, 1<<bits), reuse: snap.Checkpoint.Bits == bits}
	for index, states := range byShard {
		if layout.reuse && !slices.ContainsFunc(states, func(state *pathState) bool { return !state.unchanged() }) {
			continue
		}
		layout.shards = append(layout.shards, index)
		layout.groups[index] = make([]grouped, len(states))
		for position, state := range states {
			document := grouped{state: state, hash: writer.pathHash(state.Path)}
			layout.groups[index][position] = document
			if !state.unchanged() {
				layout.changed = append(layout.changed, document)
			}
		}
	}
	return layout
}

// foldChanged folds the changed documents' history blocks, by path.
func (writer checkpointWriter) foldChanged(ctx context.Context, changed []grouped) (map[string]*baseEntry, error) {
	var mu sync.Mutex
	entries := make(map[string]*baseEntry, len(changed))
	err := runParallel(ctx, writer.workers, changed, func(ctx context.Context, document grouped) error {
		blocks, err := foldHistory(ctx, writer.objects, document)
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
	return entries, err
}

// fold is the written shard index as an object.
func (layout shardLayout) fold(index int, entries map[string]*baseEntry) (modelObject, shardRef, error) {
	return foldShard(index, layout.bits, layout.folded(index, entries))
}

// size is fold's object length, without its validation and hash: an Encoder
// writes json.Marshal's bytes and a newline, uncopied.
func (layout shardLayout) size(index int, entries map[string]*baseEntry) (int, error) {
	var count byteCount
	if err := json.NewEncoder(&count).Encode(newFoldedShard(index, layout.bits, layout.folded(index, entries))); err != nil {
		return 0, fmt.Errorf("measure shard %s: %w", shardLabel(index, layout.bits), err)
	}
	return int(count) - 1, nil
}

// byteCount sizes an encoding without holding its bytes.
type byteCount int

func (count *byteCount) Write(data []byte) (int, error) {
	*count += byteCount(len(data))
	return len(data), nil
}

// folded prefers a changed document's newly folded base to its stored one.
func (layout shardLayout) folded(index int, entries map[string]*baseEntry) []foldedEntry {
	folded := make([]foldedEntry, len(layout.groups[index]))
	for position, document := range layout.groups[index] {
		base := entries[document.state.Path]
		if base == nil {
			base = document.state.Base
		}
		folded[position] = foldedEntryOf(document, base)
	}
	return folded
}

// fit widens layout until every written shard is within the budget.
func (writer checkpointWriter) fit(ctx context.Context, snap *snapshot, layout shardLayout, entries map[string]*baseEntry) (shardLayout, error) {
	var narrower parentSizes
	for {
		sizes, err := writer.measure(ctx, layout, entries, narrower)
		if err != nil {
			return shardLayout{}, err
		}
		position := slices.IndexFunc(layout.shards, func(index int) bool { return sizes[index] > writer.budget })
		if position < 0 {
			return layout, nil
		}
		oversized, bits := layout.shards[position], layout.bits
		if bits == maxShardBits || len(layout.groups[oversized]) == 1 {
			return shardLayout{}, fmt.Errorf("shard %s of %d documents exceeds %d bytes at %d bits", shardLabel(oversized, bits), len(layout.groups[oversized]), writer.budget, bits)
		}
		layout = writer.group(snap, bits+1)
		growth, err := headerGrowth(layout.bits)
		if err != nil {
			return shardLayout{}, err
		}
		narrower = parentSizes{sizes: sizes, growth: growth}
	}
}

// parentSizes are the sizes one width narrower, whose headers are growth
// bytes shorter than their children's.
type parentSizes struct {
	sizes  []int
	growth int
}

// errOversized stops a measure at the first shard over budget.
var errOversized = errors.New("shard over budget")

// measure sizes the written shards, bounded by parents, holding no object: a
// large world's would be every shard. -1 is unwritten, or unmeasured past a
// stop at an oversized one.
func (writer checkpointWriter) measure(ctx context.Context, layout shardLayout, entries map[string]*baseEntry, parents parentSizes) ([]int, error) {
	sizes := make([]int, 1<<layout.bits)
	for index := range sizes {
		sizes[index] = -1
	}
	pending := make([]int, 0, len(layout.shards))
	for _, index := range layout.shards {
		// A child holds a subset of its parent's entries, so only the header
		// grows: the bound needs no measure while it fits.
		if len(parents.sizes) > 0 {
			if parent := parents.sizes[index>>1]; parent >= 0 && parent+parents.growth <= writer.budget {
				sizes[index] = parent + parents.growth
				continue
			}
		}
		pending = append(pending, index)
	}
	err := runParallel(ctx, writer.workers, pending, func(_ context.Context, index int) error {
		size, err := layout.size(index, entries)
		if err != nil {
			return err
		}
		sizes[index] = size
		if size > writer.budget {
			return errOversized
		}
		return nil
	})
	if err != nil && !errors.Is(err, errOversized) {
		return nil, err
	}
	return sizes, nil
}

// headerGrowth is how much a shard's own fields grow from bits-1 to bits.
func headerGrowth(bits int) (int, error) {
	narrow, err := headerLen(bits - 1)
	if err != nil {
		return 0, err
	}
	wide, err := headerLen(bits)
	if err != nil {
		return 0, err
	}
	return wide - narrow, nil
}

// headerLen is the encoded length of an empty shard at bits.
func headerLen(bits int) (int, error) {
	data, err := json.Marshal(newFoldedShard(0, bits, []foldedEntry{}))
	if err != nil {
		return 0, fmt.Errorf("measure shard header: %w", err)
	}
	return len(data), nil
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
	shard := newFoldedShard(index, bits, entries)
	label := shard.Shard
	if err := validateFoldedShard(&shard, index, bits); err != nil {
		return modelObject{}, shardRef{}, fmt.Errorf("build shard %s: %w", label, err)
	}
	object, ref, err := immutableJSON(func(hash string) string { return shardKey(label, hash) }, shard)
	if err != nil {
		return modelObject{}, shardRef{}, fmt.Errorf("build shard %s: %w", label, err)
	}
	return object, shardRef{Shard: label, objectRef: ref}, nil
}

// newFoldedShard is the one shard value foldShard writes and size measures.
func newFoldedShard(index, bits int, entries []foldedEntry) foldedShard {
	return foldedShard{Schema: foldedSchema, ShardBits: bits, Shard: shardLabel(index, bits), Entries: entries}
}

func foldRoot(worldID string, documents, bits int, shards []shardRef) (modelObject, objectRef, error) {
	root := foldedRoot{Schema: foldedSchema, WorldID: worldID, DocumentCount: documents, ShardBits: bits, Shards: shards}
	if err := validateFoldedRoot(&root, worldID); err != nil {
		return modelObject{}, objectRef{}, fmt.Errorf("build root: %w", err)
	}
	return immutableJSON(rootKey, root)
}
