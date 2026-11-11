package bucketstore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

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
	// budget is the bytes a shard may hold before the layout widens; part
	// the most entries of one prune manifest part.
	budget int
	part   int
}

func (store *Store) checkpointWriter() checkpointWriter {
	return checkpointWriter{
		bodyReader: store.bodyReader(), workers: store.shardWorkers, now: store.now, grace: store.checkpointGrace,
		pathHash: pathHash, budget: store.shardBudget, part: store.compaction.collectLimit(),
	}
}

// write writes snap's changed history blocks and shards, its prune manifest,
// the root, the checkpoint, then the shards' segments, which the root does
// not name: a missing one costs readers blob reads and never fails it.
func (writer checkpointWriter) write(ctx context.Context, snap *snapshot) (*adoption, error) {
	objects, worldID, workers := writer.objects, writer.worldID, writer.workers
	previous := snap.Checkpoint
	// Bits never fall: a layout the count rule outgrew, or one a shard's size
	// forced, is kept, else every checkpoint would fold the world again.
	layout := writer.group(snap, max(shardBitsFor(snap.Paths.Len()), previous.Bits))
	// The changed set is the same at every width: a shard holding a change is
	// always written.
	entries, pruned, err := writer.foldChanged(ctx, layout.changed)
	if err == nil {
		layout, err = writer.fit(ctx, snap, layout, entries)
	}
	if err == nil {
		err = writer.writePruned(ctx, snap.Sequence, pruned)
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

// foldChanged folds the changed documents' history blocks, by path, and
// lists the versions they pruned since their checkpoint entries.
func (writer checkpointWriter) foldChanged(ctx context.Context, changed []grouped) (map[string]*baseEntry, []prunedEntry, error) {
	var mu sync.Mutex
	entries := make(map[string]*baseEntry, len(changed))
	var pruned []prunedEntry
	err := runParallel(ctx, writer.workers, changed, func(ctx context.Context, document grouped) error {
		blocks, documentPruned, err := writer.foldHistory(ctx, document)
		if err != nil {
			return fmt.Errorf("fold history of %s: %w", document.state.Path, err)
		}
		state := document.state
		base := &baseEntry{History: blocks, Current: state.Current, Archived: state.Archived, BodyHash: state.BodyHash, Modified: state.Modified}
		mu.Lock()
		entries[state.Path] = base
		pruned = append(pruned, documentPruned...)
		mu.Unlock()
		return nil
	})
	return entries, pruned, err
}

// writePruned writes the pruned versions, sorted so racing compactors write
// the same bytes, as manifest parts of at most part entries; collection
// deletes them, so they are created as droppable objects are.
func (writer checkpointWriter) writePruned(ctx context.Context, sequence int64, pruned []prunedEntry) error {
	slices.SortFunc(pruned, func(a, b prunedEntry) int {
		return cmp.Or(strings.Compare(a.Path, b.Path), cmp.Compare(a.Version, b.Version))
	})
	chunks, err := pruneParts(pruned, writer.part)
	if err != nil {
		return err
	}
	parts := make([]modelObject, 0, len(chunks))
	for _, entries := range chunks {
		manifest := prunedObject{Schema: prunedSchema, WorldID: writer.worldID, Sequence: sequence, Entries: entries}
		err := validatePruned(&manifest, writer.worldID, sequence)
		var object modelObject
		if err == nil {
			object, _, err = immutableJSON(func(hash string) string { return prunedKey(sequence, hash) }, manifest)
		}
		if err == nil && len(object.Data) > MaxObjectBytes {
			err = fmt.Errorf("prune manifest part is %d bytes, limit is %d", len(object.Data), MaxObjectBytes)
		}
		if err != nil {
			return fmt.Errorf("build prune manifest: %w", err)
		}
		parts = append(parts, object)
	}
	return runParallel(ctx, writer.workers, parts, func(ctx context.Context, object modelObject) error {
		return createImmutable(ctx, writer.objects, object)
	})
}

// prunePartBytes bounds a manifest part's encoded entries, below the object
// limit by room for the part's own fields.
const prunePartBytes = MaxObjectBytes - 1<<10

// pruneParts splits sorted entries into parts of at most count entries and
// prunePartBytes encoded: JSON escaping can grow a path sixfold.
func pruneParts(entries []prunedEntry, count int) ([][]prunedEntry, error) {
	var parts [][]prunedEntry
	start, size := 0, 0
	for i := range entries {
		encoded, err := json.Marshal(&entries[i])
		if err != nil {
			return nil, fmt.Errorf("measure pruned entry: %w", err)
		}
		n := len(encoded) + 1 // the comma
		if i > start && (i-start == count || size+n > prunePartBytes) {
			parts, start, size = append(parts, entries[start:i]), i, 0
		}
		size += n
	}
	if start < len(entries) {
		parts = append(parts, entries[start:])
	}
	return parts, nil
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
// retained versions, and the versions pruned since its checkpoint entry: the
// entry's below First, then the log's. It creates the blocks the entry lacks.
func (writer checkpointWriter) foldHistory(ctx context.Context, document grouped) ([]blockRef, []prunedEntry, error) {
	objects, state := writer.objects, document.state
	var base []blockRef
	baseCurrent := 0
	if state.Base != nil {
		base, baseCurrent = state.Base.History, state.Base.Current
	}
	first := state.First
	if first < 1 {
		return nil, nil, fmt.Errorf("%w: no first retained version", blob.ErrIntegrity)
	}
	pruned, err := writer.prunedBlocks(ctx, document, first)
	if err != nil {
		return nil, nil, err
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
		// The block holding First is read here, so its pruned versions come
		// from this read; blocks wholly below First came from prunedBlocks.
		if found && previous.Last >= from {
			history, err := readBlock(ctx, objects, document.hash, previous)
			if err != nil {
				return nil, nil, err
			}
			for _, entry := range history.Entries {
				if entry.Version >= from {
					versions = append(versions, entry)
				} else {
					pruned = append(pruned, prunedOf(state.Path, &entry))
				}
			}
		}
		for _, version := range state.Recent {
			if version.entry.Version > baseCurrent && version.entry.Version >= from && version.entry.Version <= to {
				versions = append(versions, version.entry)
			}
		}
		ref, err := createBlock(ctx, objects, historyObject{Schema: historySchema, PathHash: document.hash, First: from, Last: to, Entries: versions})
		if err != nil {
			return nil, nil, err
		}
		blocks = append(blocks, ref)
	}
	for node := state.Pruned; node != nil; node = node.next {
		if node.version <= baseCurrent || node.version >= first {
			return nil, nil, fmt.Errorf("%w: pruned v%d is outside (%d,%d)", blob.ErrIntegrity, node.version, baseCurrent, first)
		}
		pruned = append(pruned, prunedEntry{Path: state.Path, Version: node.version, Blob: node.blob})
	}
	return blocks, pruned, nil
}

// prunedBlocks lists the versions of the document's checkpoint blocks that
// lie wholly below first, reading and verifying the blocks in parallel.
func (writer checkpointWriter) prunedBlocks(ctx context.Context, document grouped, first int) ([]prunedEntry, error) {
	if document.state.Base == nil {
		return nil, nil
	}
	below := document.state.Base.History
	if index := slices.IndexFunc(below, func(ref blockRef) bool { return ref.Last >= first }); index >= 0 {
		below = below[:index]
	}
	dropped := make([][]prunedEntry, len(below))
	err := runParallel(ctx, writer.workers, indexes(len(below)), func(ctx context.Context, index int) error {
		history, err := readBlock(ctx, writer.objects, document.hash, below[index])
		if err != nil {
			return err
		}
		for entry := range history.Entries {
			dropped[index] = append(dropped[index], prunedOf(document.state.Path, &history.Entries[entry]))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(dropped...), nil
}

func prunedOf(path string, version *historyEntry) prunedEntry {
	return prunedEntry{Path: path, Version: version.Version, Blob: version.Blob}
}

// createBlock validates and creates one history block.
func createBlock(ctx context.Context, objects blob.Store, history historyObject) (blockRef, error) {
	if err := validateHistoryObject(&history); err != nil {
		return blockRef{}, fmt.Errorf("%w: versions %d-%d: %v", blob.ErrIntegrity, history.First, history.Last, err)
	}
	object, ref, err := immutableJSON(historyKey, history)
	if err == nil {
		err = createContent(ctx, objects, object)
	}
	if err != nil {
		return blockRef{}, err
	}
	return blockRef{First: history.First, Last: history.Last, Hash: ref.Hash}, nil
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
