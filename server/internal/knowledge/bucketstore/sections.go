package bucketstore

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"
	"time"

	"github.com/google/btree"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// The section index for body search (ADR 0012) is never built on the commit
// path or when a slot applies: built on the first body search, or warmed by
// the write gate (WarmSections); dropped when idle (ADR 0036).
const (
	// sectionIdle is how long a world keeps its index without a body search
	// or a write.
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

func newSectionIndex(store *Store) *sectionIndex {
	return &sectionIndex{store: store, idle: sectionIdle, wait: backend.SearchFreshness, published: make(chan struct{}), wake: make(chan struct{}, 1)}
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
	return index.settle(ctx, func() (*sectionVersion, bool) { return index.findLocked(sequence) })
}

// latest is the newest version built, whatever snapshot it covers, waiting
// the same bound for a cold index so a world's first writes are judged;
// nil when none in time.
func (index *sectionIndex) latest(ctx context.Context) *sectionVersion {
	version, _ := index.settle(ctx, func() (*sectionVersion, bool) {
		if n := len(index.versions); n > 0 {
			return index.versions[n-1], true
		}
		return nil, false
	})
	return version
}

// activate starts the build; a no-op while the index is active or closed.
func (index *sectionIndex) activate() {
	index.mu.Lock()
	defer index.mu.Unlock()
	if index.closed {
		return
	}
	index.activateLocked()
	index.used = index.store.now()
}

// settle answers find at once, else waits up to the index's wait for a
// publish that settles it, counted as a waiter against eviction; find runs
// under the lock. The first call starts the build.
func (index *sectionIndex) settle(ctx context.Context, find func() (*sectionVersion, bool)) (*sectionVersion, bool) {
	index.mu.Lock()
	if index.closed {
		index.mu.Unlock()
		return nil, false
	}
	index.activateLocked()
	if version, settled := find(); settled {
		index.used = index.store.now()
		index.mu.Unlock()
		return version, version != nil
	}
	deadline := time.NewTimer(index.wait)
	defer deadline.Stop()
	index.waiting++
	defer func() {
		index.mu.Lock()
		index.waiting--
		index.used = index.store.now()
		index.mu.Unlock()
	}()
	for {
		published := index.published
		index.mu.Unlock()
		select {
		case <-published:
		case <-deadline.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
		index.mu.Lock()
		if version, settled := find(); settled {
			index.mu.Unlock()
			return version, version != nil
		}
	}
}

// findLocked is the version at sequence; settled once the index has it or has
// moved past it without it.
func (index *sectionIndex) findLocked(sequence int64) (version *sectionVersion, settled bool) {
	for position, candidate := range slices.Backward(index.versions) {
		switch {
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
func (index *sectionIndex) installed(snap *snapshot, applied []appliedSlot) {
	index.mu.Lock()
	defer index.mu.Unlock()
	if !index.active || index.closed || index.resync {
		return
	}
	var changed []*pathState
	for _, slot := range applied {
		for _, event := range slot.events {
			changed = append(changed, snap.path(event.Path))
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
			index.prune()
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
	index.pruneLocked(version.published)
	close(index.published)
	index.published = make(chan struct{})
}

// prune is pruneLocked now, for a worker with nothing else to do.
func (index *sectionIndex) prune() {
	index.mu.Lock()
	defer index.mu.Unlock()
	index.pruneLocked(index.store.now())
}

// pruneLocked drops versions superseded more than a request ago: no view
// pinned to their snapshot can still ask for them.
func (index *sectionIndex) pruneLocked(now time.Time) {
	stale := 0
	for stale < len(index.versions)-1 && now.Sub(index.versions[stale+1].published) > index.store.requestTimeout {
		stale++
	}
	if stale > 0 {
		// Copied, so the old array does not keep the dropped versions alive.
		index.versions = append([]*sectionVersion(nil), index.versions[stale:]...)
	}
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
