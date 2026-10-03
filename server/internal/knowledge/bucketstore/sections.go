package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/google/btree"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// The section index for body search (ADR 0012) is never built on the commit
// path or when a slot applies: a world builds it on its first body search,
// keeps it current behind installs, and drops it when idle (ADR 0036).
const (
	// sectionIdle is how long a world keeps its index without a body search.
	sectionIdle = 30 * time.Minute
	// sectionSteps bounds the installs queued for the index; past it the
	// worker catches up to the served snapshot in one diff instead.
	sectionSteps = 1024
	// A failed build or catch-up is retried after sectionRetry, doubling to
	// sectionRetryMax while it keeps failing.
	sectionRetry    = time.Second
	sectionRetryMax = 30 * time.Second
	// sectionTimeout bounds one build or catch-up, so a hung read is retried.
	sectionTimeout = 5 * time.Minute
)

// sectionIndex keeps immutable versions of a world's section index, one per
// snapshot installed since it was built, so a body search answers exactly
// the snapshot its view pinned.
type sectionIndex struct {
	store *Store
	idle  time.Duration
	wait  time.Duration

	mu     sync.Mutex
	active bool
	closed bool
	// used is when a body search last ended; none is evicted while waiting.
	used    time.Time
	waiting int
	// versions are ascending by sequence; those older than a request may
	// still read are pruned as new ones are published.
	versions  []*sectionVersion
	steps     []sectionStep
	resync    bool
	published chan struct{} // closed and replaced on every publish
	wake      chan struct{}
	stop      context.CancelFunc
	done      chan struct{}
}

// sectionVersion is the section index of one snapshot: every live document
// with its catalog entry and sections, a catalog.Index.
type sectionVersion struct {
	sequence  int64
	docs      *btree.BTreeG[*docSections]
	published time.Time
}

// docSections is a live document in a version; sections is nil when its body
// could not be read.
type docSections struct {
	path     string
	bodyHash string
	entry    *catalog.Entry
	sections *catalog.DocSections
}

// sectionStep is an installed snapshot's sequence and the documents its
// slots changed, as it holds them.
type sectionStep struct {
	sequence int64
	changed  []*pathState
}

func newDocTree() *btree.BTreeG[*docSections] {
	return btree.NewG(treeDegree, func(a, b *docSections) bool { return a.path < b.path })
}

func docProbe(path string) *docSections { return &docSections{path: path} }

func newSectionIndex(store *Store, idle, wait time.Duration) *sectionIndex {
	return &sectionIndex{store: store, idle: idle, wait: wait, published: make(chan struct{}), wake: make(chan struct{}, 1)}
}

func (version *sectionVersion) Entries(scope string) iter.Seq2[*catalog.Entry, *catalog.DocSections] {
	return func(yield func(*catalog.Entry, *catalog.DocSections) bool) {
		ascendScope(version.docs, scope, docProbe, func(doc *docSections) bool { return yield(doc.entry, doc.sections) })
	}
}

// await returns the version built for the snapshot at sequence, waiting up to
// the index's wait for it; false when there is none in time. The first call
// starts the build.
func (index *sectionIndex) await(ctx context.Context, sequence int64) (*sectionVersion, bool) {
	deadline := time.NewTimer(index.wait)
	defer deadline.Stop()
	index.mu.Lock()
	if index.closed {
		index.mu.Unlock()
		return nil, false
	}
	index.activateLocked()
	index.waiting++
	defer func() {
		index.mu.Lock()
		index.waiting--
		index.used = index.store.now()
		index.mu.Unlock()
	}()
	for {
		version, settled := index.findLocked(sequence)
		published := index.published
		index.mu.Unlock()
		if settled {
			return version, version != nil
		}
		select {
		case <-published:
		case <-deadline.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
		index.mu.Lock()
	}
}

// findLocked is the version at sequence; settled once the index has it or has
// moved past it without it.
func (index *sectionIndex) findLocked(sequence int64) (version *sectionVersion, settled bool) {
	for position := len(index.versions) - 1; position >= 0; position-- {
		switch candidate := index.versions[position]; {
		case candidate.sequence == sequence:
			return candidate, true
		case candidate.sequence < sequence:
			return nil, position < len(index.versions)-1
		}
	}
	return nil, len(index.versions) > 0
}

func (index *sectionIndex) activateLocked() {
	if index.active {
		return
	}
	ctx, stop := context.WithCancel(context.Background())
	index.active, index.stop, index.done = true, stop, make(chan struct{})
	go index.run(ctx, index.done)
}

// resetLocked drops the index; the next body search builds it again.
func (index *sectionIndex) resetLocked() {
	index.active, index.versions, index.steps, index.resync = false, nil, nil, false
}

// boundLocked turns a queue past sectionSteps into one catch-up to the
// served snapshot.
func (index *sectionIndex) boundLocked() {
	if len(index.steps) > sectionSteps {
		index.steps, index.resync = nil, true
	}
}

// reloaded drops the queued steps of a store that started again from a
// checkpoint: the index catches up by comparing with the served snapshot.
func (index *sectionIndex) reloaded() {
	index.mu.Lock()
	defer index.mu.Unlock()
	if !index.active || index.closed {
		return
	}
	index.steps, index.resync = nil, true
	index.wakeLocked()
}

// installed queues a snapshot the store now serves; it does no section work.
func (index *sectionIndex) installed(snap *snapshot, applied []*slotObject) {
	index.mu.Lock()
	defer index.mu.Unlock()
	if !index.active || index.closed || index.resync {
		return
	}
	var changed []*pathState
	for _, slot := range applied {
		for entry := range slot.Entries {
			changed = append(changed, snap.path(slot.Entries[entry].Path))
		}
	}
	index.steps = append(index.steps, sectionStep{sequence: snap.Sequence, changed: changed})
	index.boundLocked()
	index.wakeLocked()
}

// wakeLocked tells the worker there is work, without waiting for it.
func (index *sectionIndex) wakeLocked() {
	select {
	case index.wake <- struct{}{}:
	default:
	}
}

// close stops the worker, then drops the index; every later body search
// answers from the catalog.
func (index *sectionIndex) close() {
	index.mu.Lock()
	index.closed = true
	stop, done := index.stop, index.done
	index.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	index.resetLocked()
}

// run builds the index, then applies queued installs until stopped or idle.
func (index *sectionIndex) run(ctx context.Context, done chan struct{}) {
	evicted := false
	defer func() {
		if !evicted {
			close(done)
		}
	}()
	built, retry := false, sectionRetry
	for {
		var err error
		var wake <-chan struct{}
		var delay time.Duration
		workCtx, cancel := context.WithTimeout(ctx, sectionTimeout)
		if !built {
			err = index.build(workCtx)
			built = err == nil
		} else if steps, resync := index.take(); len(steps) > 0 || resync {
			if err = index.advance(workCtx, steps, resync); err != nil {
				index.requeue(steps, resync)
			}
		} else {
			wake, delay = index.wake, min(index.idle, time.Minute)
		}
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				index.store.logger.Warn("section index behind; body search answers from the catalog until it catches up", "world", index.store.worldID, "retry", retry, "error", err)
			}
			delay, retry = retry, min(2*retry, sectionRetryMax)
		} else if wake == nil {
			retry = sectionRetry
		}
		if ctx.Err() != nil {
			return
		}
		if evicted = index.evictIfIdle(done); evicted || !index.pause(ctx, wake, delay) {
			return
		}
	}
}

// pause waits for delay, or for an install when wake is set; false once
// stopped.
func (index *sectionIndex) pause(ctx context.Context, wake <-chan struct{}, delay time.Duration) bool {
	if wake == nil && delay == 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
	case <-timer.C:
	}
	return true
}

// evictIfIdle drops the index after idle without a body search. The worker
// whose done it is closes it in the same step, so a later activation never
// overlaps it and close never waits on the wrong worker.
func (index *sectionIndex) evictIfIdle(done chan struct{}) bool {
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.waiting > 0 || index.store.now().Sub(index.used) < index.idle {
		return false
	}
	index.resetLocked()
	close(done)
	return true
}

func (index *sectionIndex) take() ([]sectionStep, bool) {
	index.mu.Lock()
	defer index.mu.Unlock()
	steps, resync := index.steps, index.resync
	index.steps, index.resync = nil, false
	return steps, resync
}

// requeue puts back steps a failed catch-up took, ahead of newer ones.
func (index *sectionIndex) requeue(steps []sectionStep, resync bool) {
	index.mu.Lock()
	defer index.mu.Unlock()
	index.steps = append(steps, index.steps...)
	index.resync = index.resync || resync
	index.boundLocked()
}

func (index *sectionIndex) newest() *sectionVersion {
	index.mu.Lock()
	defer index.mu.Unlock()
	return index.versions[len(index.versions)-1]
}

// publish adds a version and drops those no request can still be reading.
func (index *sectionIndex) publish(version *sectionVersion) {
	index.mu.Lock()
	defer index.mu.Unlock()
	version.published = index.store.now()
	index.versions = append(index.versions, version)
	stale := 0
	for stale < len(index.versions)-1 && version.published.Sub(index.versions[stale].published) > index.store.requestTimeout {
		stale++
	}
	if stale > 0 {
		// Copied, so the old array does not keep the dropped versions alive.
		index.versions = append([]*sectionVersion(nil), index.versions[stale:]...)
	}
	close(index.published)
	index.published = make(chan struct{})
}

// build indexes the served snapshot from its checkpoint's segments and blobs.
func (index *sectionIndex) build(ctx context.Context) error {
	snap := index.store.served.Load().snap
	docs, err := index.store.buildSections(ctx, snap)
	if err != nil {
		return fmt.Errorf("build section index at %d: %w", snap.Sequence, err)
	}
	index.publish(&sectionVersion{sequence: snap.Sequence, docs: docs})
	return nil
}

// advance publishes a version for each queued install, or one for the served
// snapshot after the queue overflowed.
func (index *sectionIndex) advance(ctx context.Context, steps []sectionStep, resync bool) error {
	current := index.newest()
	if resync {
		target := index.store.served.Load().snap
		if target.Sequence > current.sequence {
			next, err := index.store.resyncSections(ctx, current, target)
			if err != nil {
				return err
			}
			index.publish(next)
			current = next
		}
	}
	var pending []sectionStep
	for _, step := range steps {
		if step.sequence > current.sequence {
			pending = append(pending, step)
		}
	}
	read, err := index.store.readStepSections(ctx, current, pending)
	if err != nil {
		return err
	}
	for _, step := range pending {
		current = current.apply(step, read)
		index.publish(current)
	}
	return nil
}

// bodyKey names one body of one path.
type bodyKey struct{ path, hash string }

// readStepSections indexes every body the steps introduce, once each and in
// parallel, against the bodies current already holds.
func (store *Store) readStepSections(ctx context.Context, current *sectionVersion, steps []sectionStep) (map[bodyKey]*catalog.DocSections, error) {
	latest := make(map[string]string)
	var needs []*pathState
	for _, step := range steps {
		for _, state := range step.changed {
			if state.Archived {
				latest[state.Path] = ""
				continue
			}
			previous, seen := latest[state.Path]
			if !seen {
				if doc, ok := current.docs.Get(docProbe(state.Path)); ok {
					previous = doc.bodyHash
				}
			}
			latest[state.Path] = state.BodyHash
			if previous != state.BodyHash {
				needs = append(needs, state)
			}
		}
	}
	reader := store.bodyReader()
	var mu sync.Mutex
	read := make(map[bodyKey]*catalog.DocSections, len(needs))
	err := runParallel(ctx, store.shardWorkers, needs, func(ctx context.Context, state *pathState) error {
		body, err := reader.read(ctx, state)
		if err != nil {
			return err
		}
		var sections *catalog.DocSections
		if body != nil {
			sections = catalog.IndexSections(body)
		}
		mu.Lock()
		read[bodyKey{state.Path, state.BodyHash}] = sections
		mu.Unlock()
		return nil
	})
	return read, err
}

// apply is the version after step, with the sections read for it.
func (version *sectionVersion) apply(step sectionStep, read map[bodyKey]*catalog.DocSections) *sectionVersion {
	docs := version.docs.Clone()
	for _, state := range step.changed {
		if state.Archived {
			docs.Delete(docProbe(state.Path))
			continue
		}
		doc := &docSections{path: state.Path, bodyHash: state.BodyHash, entry: state.Entry}
		if old, ok := docs.Get(docProbe(state.Path)); ok && old.bodyHash == state.BodyHash {
			doc.sections = old.sections
		} else {
			doc.sections = read[bodyKey{state.Path, state.BodyHash}]
		}
		docs.ReplaceOrInsert(doc)
	}
	return &sectionVersion{sequence: step.sequence, docs: docs}
}

// resyncSections is current brought to target by comparing every document,
// for when installs came faster than the queue holds.
func (store *Store) resyncSections(ctx context.Context, current *sectionVersion, target *snapshot) (*sectionVersion, error) {
	step := sectionStep{sequence: target.Sequence}
	target.Paths.Ascend(func(state *pathState) bool {
		doc, ok := current.docs.Get(docProbe(state.Path))
		if state.Archived == ok || ok && (doc.bodyHash != state.BodyHash || doc.entry != state.Entry) {
			step.changed = append(step.changed, state)
		}
		return true
	})
	read, err := store.readStepSections(ctx, current, []sectionStep{step})
	if err != nil {
		return nil, err
	}
	return current.apply(step, read), nil
}

// bodyReader reads live documents' bodies for section work.
type bodyReader struct {
	objects blob.Store
	logger  *slog.Logger
	worldID string
}

func (store *Store) bodyReader() bodyReader {
	return bodyReader{objects: store.objects, logger: store.logger, worldID: store.worldID}
}

// read returns a live document's current body; nil when it is missing or
// corrupt, which is logged and skipped as ADR 0012 does. Any other failure
// is returned so the work is retried.
func (reader bodyReader) read(ctx context.Context, state *pathState) ([]byte, error) {
	view := &readView{objects: reader.objects}
	retained, err := view.currentRetained(ctx, state)
	var stored storedDocument
	if err == nil {
		_, stored, err = view.loadStored(ctx, &retained)
	}
	if errors.Is(err, blob.ErrIntegrity) || errors.Is(err, backend.ErrNotFound) {
		reader.logger.Error("section index skipped a document whose body cannot be read", "world", reader.worldID, "path", state.Path, "error", err)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read body of %s: %w", state.Path, err)
	}
	if stored.body == nil {
		return []byte{}, nil
	}
	return stored.body, nil
}

// shardGroup is a checkpoint shard's documents as a snapshot holds them.
type shardGroup struct {
	live []*pathState
	// based is set when the checkpoint holds a live document here; drifted
	// once one changed since, so the builder cannot write the segment.
	based, drifted bool
}

// buildSections indexes snap's live documents, shard by shard of its
// checkpoint: bodies its segments hold, the rest from blobs.
func (store *Store) buildSections(ctx context.Context, snap *snapshot) (*btree.BTreeG[*docSections], error) {
	layout := snap.Checkpoint
	groups := make([]shardGroup, len(layout.Shards))
	snap.Paths.Ascend(func(state *pathState) bool {
		group := &groups[shardOf(pathHash(state.Path), layout.Bits)]
		if base := state.Base; base != nil && !base.Archived {
			group.based = true
			group.drifted = group.drifted || !sameBody(state)
		}
		if !state.Archived {
			group.live = append(group.live, state)
		}
		return true
	})
	built := make([][]*docSections, len(groups))
	err := runParallel(ctx, store.shardWorkers, indexes(len(groups)), func(ctx context.Context, shard int) error {
		docs, err := store.buildShard(ctx, layout.Shards[shard], &groups[shard])
		built[shard] = docs
		return err
	})
	if err != nil {
		return nil, err
	}
	tree := newDocTree()
	for _, docs := range built {
		for _, doc := range docs {
			tree.ReplaceOrInsert(doc)
		}
	}
	return tree, nil
}

// buildShard indexes one shard's live documents. When the shard has no
// segment and nothing in it changed since the checkpoint, it writes one.
func (store *Store) buildShard(ctx context.Context, ref shardRef, group *shardGroup) ([]*docSections, error) {
	if len(group.live) == 0 {
		return nil, nil
	}
	reader := store.bodyReader()
	var carried map[string]string
	if group.based {
		var err error
		if carried, err = reader.segment(ctx, ref); err != nil {
			return nil, err
		}
	}
	bodies, err := reader.shardBodies(ctx, group.live, carried)
	if err != nil {
		return nil, err
	}
	docs := make([]*docSections, len(group.live))
	for position, state := range group.live {
		docs[position] = &docSections{path: state.Path, bodyHash: state.BodyHash, entry: state.Entry}
		if body := bodies[position]; body != nil {
			docs[position].sections = catalog.IndexSectionsString(body.Body)
		}
	}
	if carried == nil && group.based && !group.drifted && !store.readOnly {
		segment := readBodies(bodies, func(position int) bool { return sameBody(group.live[position]) })
		if err := writeSegment(ctx, store.objects, ref, segment); err != nil && ctx.Err() == nil {
			store.logger.Warn("section segment not written; the next build reads its bodies again", "world", store.worldID, "shard", ref.Hash, "error", err)
		}
	}
	return docs, nil
}
