package bucketstore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	mathrand "math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

const (
	// batchStart is the batch limit; it grows to maxSlotEntries while the
	// queue stays deeper than a batch.
	batchStart = 16
	// pipelineDepth is the slot being created and two batches staged behind
	// it; with one, staging stayed on the critical path (ADR 0036).
	pipelineDepth = 3
	// handoffWait bounds how long a winner holds its next slot for a peer
	// that lost to it; the hold ends once a peer's slot is installed.
	handoffWait = time.Second
	// ownSlots is how many of its newest slots a store remembers, to tell a
	// loser's hint from a peer's: more than a second of slots at any rate.
	ownSlots = 256
)

// jitterWithin is a random wait from (0, limit].
func jitterWithin(limit time.Duration) time.Duration {
	return time.Duration(mathrand.Int64N(int64(limit)) + 1) //nolint:gosec // a backoff jitter, not a secret
}

// commitQueue holds the mutations waiting for this store's committer, which
// runs while there is work and exits when idle.
type commitQueue struct {
	mu      sync.Mutex
	waiting []*commitRequest
	running bool
	closed  bool
	wake    chan struct{} // a request arrived or Close ran
	done    sync.WaitGroup
}

// commitRequest is one mutation waiting for its outcome.
type commitRequest struct {
	ctx         context.Context // the request's, bounded by the request timeout
	path        string          // canonical path it changes
	build       mutationBuilder
	operationID string
	answer      chan commitAnswer // buffered: the committer never waits on it
	// taken is set once a batch holds the request: past it, a request that
	// gives up cannot say whether it committed.
	taken atomic.Bool
	// staged are the object keys created for it; the committer's alone.
	staged []string
	// warm reads what its build reads first while it waits; nil for a new path.
	warm *warmup
}

// commitAnswer is a mutation's outcome.
type commitAnswer struct {
	result mutationResult
	err    error
}

// runMutation queues one mutation and waits for its outcome.
func (store *Store) runMutation(ctx context.Context, path string, build mutationBuilder) (mutationResult, error) {
	if store.readOnly {
		return mutationResult{}, backend.ErrReadOnly
	}
	ctx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	operationID, err := randomOperationID()
	if err != nil {
		return mutationResult{}, fmt.Errorf("create operation ID: %w", err)
	}
	request := &commitRequest{
		ctx: ctx, path: path, build: build, operationID: operationID, answer: make(chan commitAnswer, 1),
		warm: store.warm(ctx, path),
	}
	if err := store.commits.enqueue(store, request); err != nil {
		store.unwarm(request)
		return mutationResult{}, err
	}
	select {
	case answer := <-request.answer:
		return answer.result, answer.err
	case <-ctx.Done():
		// A request still queued is dropped untouched once its context ends.
		if request.taken.Load() {
			return mutationResult{}, fmt.Errorf("operation %s outcome unknown: %w", operationID, ctx.Err())
		}
		return mutationResult{}, fmt.Errorf("operation %s wait for commit: %w", operationID, ctx.Err())
	}
}

func (queue *commitQueue) enqueue(store *Store, request *commitRequest) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed {
		return backend.ErrClosed
	}
	queue.waiting = append(queue.waiting, request)
	if queue.running {
		queue.signal()
		return nil
	}
	queue.running = true
	queue.done.Add(1)
	go store.commit()
	return nil
}

func (queue *commitQueue) signal() {
	select {
	case queue.wake <- struct{}{}:
	default:
	}
}

// close refuses later writes and waits for the committer to finish.
func (queue *commitQueue) close() {
	queue.mu.Lock()
	queue.closed = true
	queue.mu.Unlock()
	queue.signal()
	queue.done.Wait()
}

// active reports a committer running, which may hold snapshots of any age.
func (queue *commitQueue) active() bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.running
}

func (queue *commitQueue) isClosed() bool {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.closed
}

// committer turns queued mutations into slots: it builds batches in queue
// order, stages their objects, and creates one slot at a time while the
// batches behind it build and stage.
type committer struct {
	store *Store
	// pipeline holds the batches not yet answered, oldest first; only the
	// first creates its slot.
	pipeline []*batch
	// events are results of staging and create goroutines, run here.
	events  chan func()
	running int // goroutines that have not reported
	limit   int
	timer   *time.Timer
	// handedOver is set when a hold ended on a peer's slot, which the
	// staged head was not built on.
	handedOver bool
}

// handoff holds the next create for a peer that lost to this store, until a
// peer slot is installed or until passes; it outlives an idle committer.
// signal is the waiting hint the latest hold answered, so each holds once.
type handoff struct {
	until     time.Time
	peerSlots int64
	signal    *peerWaiting
}

// peerWaiting is a peer's hint for a slot this store wrote: the peer lost
// that slot and is retrying. peerSlots is the store's count when it came.
type peerWaiting struct {
	at        time.Time
	peerSlots int64
}

// commit runs the committer until the queue and the pipeline are empty.
func (store *Store) commit() {
	c := &committer{store: store, events: make(chan func(), pipelineDepth), limit: batchStart}
	for {
		closing := store.commits.isClosed()
		if closing {
			c.shutdown()
		}
		// The staged head's create starts before the next batch builds.
		wait := c.launch()
		if !closing {
			c.fill()
			wait = c.launch()
		}
		if c.exit() {
			return
		}
		select {
		case <-store.commits.wake:
		case event := <-c.events:
			c.running--
			event()
		case <-wait:
		}
	}
}

// exit stops the committer when nothing is left to do; under the queue's
// lock, so a request queued meanwhile starts another.
func (c *committer) exit() bool {
	if len(c.pipeline) > 0 || c.running > 0 {
		return false
	}
	queue := &c.store.commits
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.waiting) > 0 {
		return false
	}
	queue.running = false
	queue.done.Done()
	// An idle store holds no batch's objects.
	c.store.newestBatch.Store(nil)
	if c.timer != nil {
		c.timer.Stop()
	}
	return true
}

// shutdown refuses everything but a slot being created, which ends as it
// would have.
func (c *committer) shutdown() {
	queue := &c.store.commits
	queue.mu.Lock()
	waiting := queue.waiting
	queue.waiting = nil
	queue.mu.Unlock()
	for _, request := range waiting {
		c.reply(request, commitAnswer{err: backend.ErrClosed})
	}
	keep := 0
	if len(c.pipeline) > 0 && c.pipeline[0].creating {
		keep = 1
	}
	for _, b := range c.pipeline[keep:] {
		c.answerRest(b, backend.ErrClosed)
	}
	c.pipeline = c.pipeline[:keep]
}

// take removes the next batch's requests from the queue, answering those
// whose context already ended.
func (c *committer) take() []*commitRequest {
	queue := &c.store.commits
	queue.mu.Lock()
	defer queue.mu.Unlock()
	var taken []*commitRequest
	for len(queue.waiting) > 0 && len(taken) < c.limit {
		request := queue.waiting[0]
		queue.waiting[0] = nil
		queue.waiting = queue.waiting[1:]
		request.taken.Store(true)
		if err := request.ctx.Err(); err != nil {
			c.reply(request, commitAnswer{err: fmt.Errorf("operation %s wait for commit: %w", request.operationID, err)})
			continue
		}
		taken = append(taken, request)
	}
	switch {
	case len(taken) == c.limit && len(queue.waiting) > 0:
		c.limit = maxSlotEntries
	case len(taken) < batchStart:
		c.limit = batchStart
	}
	return taken
}

// requeue puts requests back at the head of the queue, in order.
func (c *committer) requeue(requests []*commitRequest) {
	if len(requests) == 0 {
		return
	}
	queue := &c.store.commits
	queue.mu.Lock()
	queue.waiting = append(requests, queue.waiting...)
	queue.mu.Unlock()
}

// fill builds batches while the pipeline has room: the first on the log's
// tip, the rest on the batch before them.
func (c *committer) fill() {
	for len(c.pipeline) < pipelineDepth {
		requests := c.take()
		if len(requests) == 0 {
			return
		}
		head := len(c.pipeline) == 0
		b := &batch{}
		if head {
			ctx, cancel := context.WithTimeout(context.Background(), c.store.requestTimeout)
			tip, err := c.store.refresh(ctx)
			cancel()
			if err != nil {
				for _, request := range requests {
					c.reply(request, commitAnswer{err: fmt.Errorf("operation %s refresh: %w", request.operationID, err)})
				}
				continue
			}
			b.base = tip
		} else {
			// Rebased once here, a checkpoint adopted meanwhile is not rebased
			// again on every later install.
			b.base = c.store.rebased(c.pipeline[len(c.pipeline)-1].next)
		}
		c.build(b, requests)
		c.pipeline = append(c.pipeline, b)
		if head {
			c.answerSettled(b)
		}
	}
}

// launch answers batches with nothing to create, then starts the first
// slot's create once it is staged; it returns a timer while a handoff holds
// it. After a handoff the head is rebuilt on the peer's slot once.
func (c *committer) launch() <-chan time.Time {
	for len(c.pipeline) > 0 {
		head := c.pipeline[0]
		if head.slot == nil {
			c.answerRest(head, nil)
			c.pop()
			continue
		}
		if head.creating || !head.staged {
			return nil
		}
		if wait := c.held(); wait > 0 {
			return c.after(wait)
		}
		if c.handedOver {
			c.handedOver = false
			if served := c.store.served.Load().snap; served.Sequence > head.base.Sequence {
				c.rebuild(head, served)
				continue
			}
		}
		c.create(head)
		return nil
	}
	return nil
}

// held is how much longer the handoff holds the next create; zero once a
// peer's slot is installed or the hold has passed.
func (c *committer) held() time.Duration {
	hold := &c.store.hold
	if hold.until.IsZero() {
		return 0
	}
	wait := time.Until(hold.until)
	if handed := c.store.peerSlots.Load() != hold.peerSlots; wait <= 0 || handed {
		hold.until, c.handedOver = time.Time{}, handed
		return 0
	}
	return wait
}

// handOff starts a hold after a win when a peer recently hinted that it lost
// to one of this store's slots and no peer slot has been installed since.
func (c *committer) handOff() {
	store := c.store
	signal := store.waiting.Load()
	if signal == nil || signal == store.hold.signal || time.Since(signal.at) > store.handoffWait || store.peerSlots.Load() != signal.peerSlots {
		return
	}
	store.hold = handoff{until: time.Now().Add(store.handoffWait), peerSlots: signal.peerSlots, signal: signal}
}

func (c *committer) after(wait time.Duration) <-chan time.Time {
	if c.timer == nil {
		c.timer = time.NewTimer(wait)
	} else {
		c.timer.Reset(wait)
	}
	return c.timer.C
}

// pop drops the answered first batch; the next one's base now exists.
func (c *committer) pop() {
	c.pipeline = slices.Delete(c.pipeline, 0, 1)
	if len(c.pipeline) > 0 {
		c.answerSettled(c.pipeline[0])
	}
}

// create starts the first batch's slot create, bounded by the latest
// deadline among its members. None starts once Close has come.
func (c *committer) create(b *batch) {
	store := c.store
	if store.commits.isClosed() {
		return
	}
	b.creating, b.started = true, store.now()
	deadline := time.Now()
	for _, m := range b.members {
		if until, ok := m.request.ctx.Deadline(); ok && m.answer == nil && until.After(deadline) {
			deadline = until
		}
	}
	store.pending.Store(&pendingSlot{base: b.base, next: b.next, hash: b.next.Tip})
	slot := modelObject{Key: slotKey(b.slot.First), Data: b.data}
	c.running++
	go func() {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		winner, err := createSlot(ctx, store.objects, slot)
		c.events <- func() { c.created(b, winner, err) }
	}()
}

// createSlot creates a slot, never rewriting it: equal bytes under its name
// are this create's own, other bytes another writer's win.
func createSlot(ctx context.Context, objects blob.Store, slot modelObject) (*blob.Object, error) {
	return createOrRead(ctx, objects, slot, false)
}

// created settles the first batch's create: confirmed, lost to winner, or
// failed with its outcome unknown.
func (c *committer) created(b *batch, winner *blob.Object, err error) {
	c.store.pending.Store(nil)
	switch {
	case err != nil:
		c.fail(b, fmt.Errorf("create slot %d: %w", b.slot.First, err))
	case winner != nil:
		c.rebase(b, winner)
	default:
		c.confirm(b)
	}
}

// confirm installs a created slot and answers its batch. It installs only
// over the slot's own base: a refresh that read the slot first did already.
func (c *committer) confirm(b *batch) {
	store := c.store
	store.installMu.Lock()
	if store.served.Load().snap.Sequence == b.base.Sequence {
		store.install(b.next, []appliedSlot{appliedOf(b.slot)}, b.started)
	}
	store.installMu.Unlock()
	for _, m := range b.members {
		if m.answer == nil {
			m.answer = &commitAnswer{result: m.result}
		}
	}
	c.answerRest(b, nil)
	c.pop()
	store.remember(b.slot.First, b.slot.last())
	store.hint(b.slot.last())
	c.handOff()
}

// fail answers every member of b that is still waiting with err, and sends
// the batches built on it back to the queue.
func (c *committer) fail(b *batch, err error) {
	index := slices.Index(c.pipeline, b)
	c.discardAfter(index)
	c.answerRest(b, err)
	c.pipeline = slices.Delete(c.pipeline, index, index+1)
}

// discardAfter sends the requests of every batch after index back to the
// queue: they were built on a state that will not exist.
func (c *committer) discardAfter(index int) {
	var requests []*commitRequest
	for _, b := range c.pipeline[index+1:] {
		for _, m := range b.members[b.answered:] {
			requests = append(requests, m.request)
		}
	}
	c.pipeline = c.pipeline[:index+1]
	c.requeue(requests)
}

// reply answers a request; each is answered once.
func (c *committer) reply(request *commitRequest, answer commitAnswer) {
	c.store.unwarm(request)
	select {
	case request.answer <- answer:
	default:
		c.store.logger.Error("commit answered twice", "operation", request.operationID, "path", request.path)
	}
}

// answerSettled sends the answers at the front of a batch whose base exists:
// those judged on the base alone.
func (c *committer) answerSettled(b *batch) {
	for b.answered < len(b.members) && b.members[b.answered].answer != nil {
		c.reply(b.members[b.answered].request, *b.members[b.answered].answer)
		b.answered++
	}
}

// answerRest sends every remaining answer, with err in place of each when
// it is set.
func (c *committer) answerRest(b *batch, err error) {
	for _, m := range b.members[b.answered:] {
		switch {
		case err != nil:
			c.reply(m.request, commitAnswer{err: err})
		case m.answer != nil:
			c.reply(m.request, *m.answer)
		default:
			c.reply(m.request, commitAnswer{err: fmt.Errorf("operation %s: committer left it without an answer", m.request.operationID)})
		}
	}
	b.answered = len(b.members)
}

func randomOperationID() (string, error) {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}
