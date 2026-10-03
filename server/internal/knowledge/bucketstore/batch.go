package bucketstore

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
)

// batch is one slot's worth of requests, built in queue order on base.
type batch struct {
	base *snapshot
	// next is base with the slot applied; base while no member is in it.
	next     *snapshot
	members  []*member
	answered int // members answered so far, a prefix
	// slot is nil when no member is in it.
	slot     *slotObject
	data     []byte
	objects  *batchObjects
	staged   bool
	creating bool
	started  time.Time // when the create began: no later slot existed then
}

// member is one request's part in a batch: an entry in the slot, or an answer
// that waits until the state it was judged on is known to exist.
type member struct {
	request *commitRequest
	deps    *dependencies
	// answer is nil while the member is in the slot.
	answer    *commitAnswer
	candidate *candidateMutation
	result    mutationResult
	// before and after are the document's state around its entry.
	before, after *pathState
}

// build builds every request on a chain from b.base, each seeing the
// members before it, then seals the slot and starts staging.
func (c *committer) build(b *batch, requests []*commitRequest) {
	b.objects = newBatchObjects(c.store.objects, c.store.newestBatch.Load(), c.pipeline)
	c.store.newestBatch.Store(b.objects)
	c.gather(b, requests)
	chain := c.store.derive(b.base)
	for _, request := range requests {
		b.members = append(b.members, c.buildMember(chain, b.objects, request))
	}
	c.seal(b, chain)
}

// warmup is what a queued write's build reads first, the document's history
// and tip as the served snapshot had them, read while the write waits; writes
// queued for one document state share it until a batch takes it.
type warmup struct {
	state   *pathState
	done    chan struct{}
	objects *batchObjects
}

// warm starts reading what a write to path builds on; nil for a new path.
func (store *Store) warm(ctx context.Context, path string) *warmup {
	state := store.served.Load().snap.path(path)
	if state == nil {
		return nil
	}
	store.warmMu.Lock()
	defer store.warmMu.Unlock()
	if w := store.warming[path]; w != nil && w.state == state {
		return w
	}
	source := newestFirst{batch: store.newestBatch.Load(), bucket: store.objects}
	w := &warmup{state: state, done: make(chan struct{}), objects: newBatchObjects(source, nil, nil)}
	store.warming[path] = w
	go store.runWarmup(ctx, path, w)
	return w
}

// runWarmup reads, at most shardWorkers at a time across the store.
func (store *Store) runWarmup(ctx context.Context, path string, w *warmup) {
	defer close(w.done)
	select {
	case store.warmSlots <- struct{}{}:
		defer func() { <-store.warmSlots }()
	case <-ctx.Done():
		return
	}
	view := &readView{objects: w.objects}
	history, err := view.history(ctx, w.state)
	if err == nil {
		_, err = view.loadBlobUnchecked(ctx, history[len(history)-1].entry.Blob)
	}
	// Only a warm-up: the build reads again and reports it.
	if err != nil {
		store.logger.Debug("commit warm-up failed", "path", path, "error", err)
	}
}

// newestFirst reads what the committer's newest batch holds before the bucket.
type newestFirst struct {
	batch  *batchObjects
	bucket objectGetter
}

func (s newestFirst) Get(ctx context.Context, key string) (blob.Object, error) {
	if s.batch != nil {
		if object, ok := s.batch.peek(key); ok {
			return object, nil
		}
	}
	return s.bucket.Get(ctx, key)
}

// gather gives the batch what its requests' warm-ups read, waiting for any
// still reading; a request keeps none of it after.
func (c *committer) gather(b *batch, requests []*commitRequest) {
	for _, request := range requests {
		if request.warm == nil {
			continue
		}
		select {
		case <-request.warm.done:
			b.objects.adopt(request.warm.objects)
		case <-request.ctx.Done():
		}
		c.store.unwarm(request)
	}
}

// unwarm ends a request's share of its warm-up: once a batch has it, or the
// request is answered without one.
func (store *Store) unwarm(request *commitRequest) {
	if request.warm == nil {
		return
	}
	store.warmMu.Lock()
	if store.warming[request.path] == request.warm {
		delete(store.warming, request.path)
	}
	store.warmMu.Unlock()
	request.warm = nil
}

// buildMember builds one request on chain and, when it changes the world,
// applies its entry there for the members after it.
func (c *committer) buildMember(chain *snapshot, objects *batchObjects, request *commitRequest) *member {
	m := &member{request: request, deps: &dependencies{}}
	if err := request.ctx.Err(); err != nil {
		m.answer = &commitAnswer{err: fmt.Errorf("operation %s: %w", request.operationID, err)}
		return m
	}
	view := &readView{objects: objects, snapshot: chain, deps: m.deps}
	candidate, result, err := request.build(request.ctx, view, request.operationID)
	if err != nil || candidate == nil {
		m.answer = &commitAnswer{result: result, err: err}
		return m
	}
	old := chain.path(candidate.entry.Path)
	state, err := chain.entryState(old, &candidate.entry)
	if err != nil {
		m.answer = &commitAnswer{err: fmt.Errorf("operation %s: change would not apply: %w", request.operationID, err)}
		return m
	}
	chain.put(old, state)
	objects.hold(candidate.objects)
	m.candidate, m.result, m.before, m.after = candidate, result, old, state
	return m
}

// keep reapplies a member whose dependencies still hold on chain, judged as
// a reader replays its entry.
func keep(chain *snapshot, m *member) bool {
	if m.request.ctx.Err() != nil || !m.deps.hold(chain) {
		return false
	}
	if m.answer != nil {
		return true
	}
	if chain.path(m.candidate.entry.Path) != m.before {
		return false
	}
	if _, err := chain.entryState(m.before, &m.candidate.entry); err != nil {
		return false
	}
	chain.put(m.before, m.after)
	return true
}

// seal makes the slot from the members in it and starts staging; every
// entry in chain was judged as a reader replays it, so the slot applies.
func (c *committer) seal(b *batch, chain *snapshot) {
	store := c.store
	b.slot, b.data, b.next, b.staged, b.creating = nil, nil, b.base, false, false
	var entries []slotEntry
	last := make(map[string]*member)
	for _, m := range b.members {
		if m.answer == nil {
			entries = append(entries, m.candidate.entry)
			last[m.candidate.entry.Path] = m
		}
	}
	if len(entries) == 0 {
		return
	}
	slot := &slotObject{
		Schema: logSchema, WorldID: store.worldID, First: b.base.Sequence + 1, Store: store.id, Prev: b.base.Tip,
		Entries: entries,
	}
	data, err := marshalImmutable(slot)
	if err == nil {
		err = validateSlot(slot, slot.First)
	}
	if err != nil {
		// Every answer was judged on a chain this slot was part of.
		for _, m := range b.members[b.answered:] {
			m.answer = &commitAnswer{err: fmt.Errorf("operation %s build slot: %w", m.request.operationID, err)}
		}
		return
	}
	chain.Sequence, chain.Tip = slot.last(), hashHex(data)
	b.slot, b.data, b.next = slot, data, chain
	c.stage(b)
}

// stage creates the objects of b's members that are not created yet.
func (c *committer) stage(b *batch) {
	store := c.store
	var objects []modelObject
	for _, m := range b.members {
		if m.answer != nil {
			continue
		}
		for _, object := range m.candidate.objects {
			if !slices.Contains(m.request.staged, object.Key) {
				objects = append(objects, object)
			}
		}
	}
	c.running++
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), store.requestTimeout)
		defer cancel()
		err := runParallel(ctx, store.shardWorkers, objects, func(ctx context.Context, object modelObject) error {
			return createImmutable(ctx, store.objects, object)
		})
		c.events <- func() { c.stagedResult(b, err) }
	}()
}

// stagedResult remembers what was created, even for a batch no longer in the
// pipeline, so a rebuilt member never stages it again.
func (c *committer) stagedResult(b *batch, err error) {
	if err == nil {
		for _, m := range b.members {
			if m.answer != nil {
				continue
			}
			for _, object := range m.candidate.objects {
				if !slices.Contains(m.request.staged, object.Key) {
					m.request.staged = append(m.request.staged, object.Key)
				}
			}
		}
	}
	if !slices.Contains(c.pipeline, b) {
		return
	}
	if err != nil {
		c.fail(b, fmt.Errorf("stage: %w", err))
		return
	}
	b.staged = true
}

// rebase moves the first batch past a slot another store won: members whose
// dependencies still hold are kept as built, the rest rebuilt, and the batch
// seals the next name. Nothing staged is staged again.
func (c *committer) rebase(b *batch, existing *blob.Object) {
	store := c.store
	c.discardAfter(0)
	winner, err := parseSlot(existing, store.worldID, b.slot.First)
	if err != nil {
		c.fail(b, fmt.Errorf("read slot %d: %w", b.slot.First, err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.requestTimeout)
	tip, err := store.catchUpFrom(ctx, winner)
	cancel()
	if err != nil {
		c.fail(b, fmt.Errorf("operation refresh: %w", err))
		return
	}
	chain := store.derive(tip)
	for index := b.answered; index < len(b.members); index++ {
		if !keep(chain, b.members[index]) {
			b.members[index] = c.buildMember(chain, b.objects, b.members[index].request)
		}
	}
	b.base = tip
	c.seal(b, chain)
	c.answerSettled(b)
}

// written is what b's members wrote, which batches built on it read before
// it is staged.
func (b *batch) written() []modelObject {
	var objects []modelObject
	for _, m := range b.members {
		if m.answer == nil {
			objects = append(objects, m.candidate.objects...)
		}
	}
	return objects
}

// objectGetter reads stored objects, all a view needs.
type objectGetter interface {
	Get(ctx context.Context, key string) (blob.Object, error)
}

// batchObjects serves a batch's reads: what it and the unconfirmed batches
// before it wrote, which may not be staged yet, and what it and the batch
// before it read. All of them are immutable.
type batchObjects struct {
	source objectGetter
	mu     sync.Mutex
	// held is what this batch read or wrote; inherited only what the batches
	// before it did, so an object nobody reads leaves after two batches.
	held, inherited map[string]blob.Object
}

// stagedModified stands in for an object's creation time until it exists.
var stagedModified = time.Unix(1, 0).UTC()

func newBatchObjects(source objectGetter, previous *batchObjects, pipeline []*batch) *batchObjects {
	objects := &batchObjects{source: source, held: make(map[string]blob.Object), inherited: make(map[string]blob.Object)}
	if previous != nil {
		previous.mu.Lock()
		maps.Copy(objects.inherited, previous.held)
		previous.mu.Unlock()
	}
	for _, earlier := range pipeline {
		for _, object := range earlier.written() {
			objects.inherited[object.Key] = writtenObject(object)
		}
	}
	return objects
}

func writtenObject(object modelObject) blob.Object {
	return blob.Object{
		Data:       object.Data,
		Attributes: blob.Attributes{Key: object.Key, Generation: 1, Size: int64(len(object.Data)), Modified: stagedModified},
	}
}

func (objects *batchObjects) hold(written []modelObject) {
	objects.mu.Lock()
	defer objects.mu.Unlock()
	for _, object := range written {
		objects.held[object.Key] = writtenObject(object)
	}
}

// adopt takes what another batch's objects read.
func (objects *batchObjects) adopt(from *batchObjects) {
	from.mu.Lock()
	read := maps.Clone(from.held)
	from.mu.Unlock()
	objects.mu.Lock()
	defer objects.mu.Unlock()
	maps.Copy(objects.held, read)
}

// peek returns an object the batch holds, without reading the bucket.
func (objects *batchObjects) peek(key string) (blob.Object, bool) {
	objects.mu.Lock()
	defer objects.mu.Unlock()
	if object, ok := objects.held[key]; ok {
		return object, true
	}
	object, ok := objects.inherited[key]
	return object, ok
}

func (objects *batchObjects) Get(ctx context.Context, key string) (blob.Object, error) {
	objects.mu.Lock()
	object, ok := objects.held[key]
	if !ok {
		if object, ok = objects.inherited[key]; ok {
			objects.held[key] = object
		}
	}
	objects.mu.Unlock()
	if ok {
		return object, nil
	}
	object, err := objects.source.Get(ctx, key)
	if err != nil {
		return blob.Object{}, err
	}
	objects.mu.Lock()
	objects.held[key] = object
	objects.mu.Unlock()
	return object, nil
}

// dependencies is what one member's build read of its chain, through its view;
// a rebase keeps the member while all of it holds. The document count the quota
// reads is left out: the quota is approximate.
type dependencies struct {
	mu sync.Mutex
	// paths holds the state each path had, nil when absent; dirs whether a
	// path was a directory.
	paths map[string]*pathState
	dirs  map[string]bool
	// broad is set by a listing or catalog read, which any change may alter.
	broad bool
}

func (deps *dependencies) sawPath(path string, state *pathState) {
	if deps == nil {
		return
	}
	deps.mu.Lock()
	defer deps.mu.Unlock()
	if deps.paths == nil {
		deps.paths = make(map[string]*pathState)
	}
	if _, seen := deps.paths[path]; !seen {
		deps.paths[path] = state
	}
}

func (deps *dependencies) sawDirectory(path string, isDir bool) {
	if deps == nil {
		return
	}
	deps.mu.Lock()
	defer deps.mu.Unlock()
	if deps.dirs == nil {
		deps.dirs = make(map[string]bool)
	}
	if _, seen := deps.dirs[path]; !seen {
		deps.dirs[path] = isDir
	}
}

func (deps *dependencies) sawAll() {
	if deps == nil {
		return
	}
	deps.mu.Lock()
	defer deps.mu.Unlock()
	deps.broad = true
}

// hold reports whether s answers every recorded read as the chain did. A
// change replaces a document's state, so the same pointer is the same state.
func (deps *dependencies) hold(s *snapshot) bool {
	if deps == nil {
		return false
	}
	deps.mu.Lock()
	defer deps.mu.Unlock()
	if deps.broad {
		return false
	}
	for path, state := range deps.paths {
		if s.path(path) != state {
			return false
		}
	}
	for path, isDir := range deps.dirs {
		if s.isDirectory(path) != isDir {
			return false
		}
	}
	return true
}
