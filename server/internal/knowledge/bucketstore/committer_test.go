package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	mathrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// Requests queued behind a full pipeline commit as one batch in queue order: a
// refusal judged against earlier members waits for the slot, and a lost slot
// keeps members the winner left alone, unjudged again, and rebuilds the rest.
func TestBatchCommitsInQueueOrder(t *testing.T) {
	objects := initializedMemory(t)
	staged := &blobCreates{Store: objects, created: make(chan struct{}, 8)}
	holds := newSlotHolds(t, staged, 2, 5)
	var hinted []int64
	var hintMu sync.Mutex
	store, err := Open(context.Background(), holds, Options{Logger: discardLogger, WorldID: testWorldID, noHedge: true, Committed: func(sequence int64) {
		hintMu.Lock()
		hinted = append(hinted, sequence)
		hintMu.Unlock()
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeAtEnd(t, store)
	write := func(path string, expected int, precondition backend.Precondition) <-chan writeOutcome {
		return publishAsync(context.Background(), store, backend.WriteRequest{
			Path: path, ExpectedVersion: expected, Content: []byte("# " + path + "\n"), Precondition: precondition,
		})
	}
	ahead := fillPipeline(t, holds, staged, func(path string) <-chan writeOutcome { return write(path, 0, nil) })
	var judged atomic.Int64
	counted := func(context.Context, backend.Reader, storefmt.PreparedWrite) error {
		judged.Add(1)
		return nil
	}
	kept := write("/kept.md", 0, counted)
	waitQueued(t, store, 1)
	contested := write("/contested.md", 0, nil)
	waitQueued(t, store, 2)
	repeated := write("/contested.md", 0, nil)
	waitQueued(t, store, 3)
	later := write("/later.md", 0, nil)
	waitQueued(t, store, 4)
	holds.holds[2].open()

	// The four queued requests build into slot 5; another store takes that
	// name with a write to /contested.md first.
	waitForTestSignal(t, holds.holds[5].arrived, "slot 5's create")
	peer := (&bucketSite{objects: objects}).open(t, 0)
	if _, err := peer.WriteVersion("/contested.md", 0, []byte("# peer\n"), nil); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	select {
	case outcome := <-repeated:
		t.Fatalf("a refusal judged behind a member came before its slot: %+v", outcome)
	case <-time.After(20 * time.Millisecond):
	}
	holds.holds[5].open()

	for name, result := range map[string]<-chan writeOutcome{"first": ahead[0], "ahead-1": ahead[1], "ahead-2": ahead[2], "kept": kept, "later": later} {
		if outcome := <-result; outcome.err != nil || outcome.document.Version != 1 {
			t.Errorf("%s = %+v, %v; want v1", name, outcome.document, outcome.err)
		}
	}
	for name, result := range map[string]<-chan writeOutcome{"contested": contested, "repeated": repeated} {
		if outcome := <-result; !errors.Is(outcome.err, storefmt.ErrConflict) {
			t.Errorf("%s = %v, want a conflict with the peer's write", name, outcome.err)
		}
	}
	if n := judged.Load(); n != 1 {
		t.Errorf("precondition of a member the winner left alone ran %d times, want 1", n)
	}
	// One for each write that built a version: the rebase stages nothing again.
	if n := staged.attempts.Load(); n != 6 {
		t.Errorf("blob creates = %d, want 6", n)
	}
	for _, path := range []string{"/kept.md", "/later.md"} {
		if state := store.served.Load().snap.path(path); state == nil || state.Sections == nil {
			t.Errorf("%s served without its sections after the rebase", path)
		}
	}
	read, err := readSlot(context.Background(), objects, testWorldID, 6)
	if err != nil {
		t.Fatalf("read slot 6: %v", err)
	}
	paths := make([]string, 0, len(read.slot.Entries))
	for _, entry := range read.slot.Entries {
		paths = append(paths, entry.Path)
	}
	if !slices.Equal(paths, []string{"/kept.md", "/later.md"}) {
		t.Errorf("slot 6 holds %v, want the kept members in queue order", paths)
	}
	hintMu.Lock()
	defer hintMu.Unlock()
	// Members hint from their own goroutines, in any order.
	slices.Sort(hinted)
	if !slices.Equal(hinted, []int64{2, 3, 4, 7}) {
		t.Errorf("hinted sequences %v, want each slot's last once", hinted)
	}
}

// A request still queued when its deadline passes gives up and never
// commits; the queue is bounded by the request deadline.
func TestQueuedWriteExpires(t *testing.T) {
	objects := initializedMemory(t)
	staged := &blobCreates{Store: objects, created: make(chan struct{}, 8)}
	holds := newSlotHolds(t, staged, 2)
	store := (&bucketSite{objects: holds, noHedge: true}).open(t, 0)
	publish := func(ctx context.Context, path string) <-chan writeOutcome {
		return publishAsync(ctx, store, backend.WriteRequest{Path: path, ExpectedVersion: 0, Content: []byte("# " + path + "\n")})
	}
	ahead := fillPipeline(t, holds, staged, func(path string) <-chan writeOutcome { return publish(context.Background(), path) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if outcome := <-publish(ctx, "/expired.md"); !errors.Is(outcome.err, context.DeadlineExceeded) {
		t.Fatalf("queued write past its deadline = %v, want deadline exceeded", outcome.err)
	}
	holds.holds[2].open()
	for _, result := range ahead {
		if outcome := <-result; outcome.err != nil {
			t.Fatalf("write: %v", outcome.err)
		}
	}
	waitIdle(t, store)
	assertCurrentVersion(t, store, "/expired.md", 0)
}

// Stores sharing one bucket commit concurrently, pipelining and racing for
// slot names: they lose no write (storetest.ConcurrentWriters), and every store
// ends on the same log.
func TestStoresShareOneLog(t *testing.T) {
	for _, count := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d stores", count), func(t *testing.T) {
			objects := &raceCounter{Store: slowStore{Store: initializedMemory(t)}}
			site := &bucketSite{objects: objects}
			stores := make([]*Store, count)
			writers := make([]storetest.Direct, count)
			for index := range stores {
				stores[index] = site.open(t, 0)
				writers[index] = storetest.Direct{Store: stores[index]}
			}
			checker := site.open(t, 0)
			storetest.ConcurrentWriters(t, writers, 8, storetest.Direct{Store: checker})
			for _, store := range stores {
				if _, err := store.refresh(context.Background()); err != nil {
					t.Fatalf("refresh: %v", err)
				}
				if store.servedSequence() != checker.servedSequence() {
					t.Errorf("store at sequence %d, a fresh one at %d", store.servedSequence(), checker.servedSequence())
				}
			}
			writersSeen, batched := logShape(t, objects, checker.servedSequence())
			if writersSeen != count || objects.losses.Load() == 0 || !batched {
				t.Errorf("log written by %d stores, %d lost creates, batched %t: the race was not exercised", writersSeen, objects.losses.Load(), batched)
			}
		})
	}
}

// A store yields after each win only while it has seen another store's slot
// within yieldWindow; alone, it never waits.
func TestYieldAfterWin(t *testing.T) {
	objects := initializedMemory(t)
	site := &bucketSite{objects: objects}
	store := site.open(t, 0)
	clock := time.Now()
	store.now = func() time.Time { return clock }
	var yields atomic.Int64
	store.jitter = func() time.Duration {
		yields.Add(1)
		return time.Millisecond
	}
	write := func(path string) {
		t.Helper()
		if _, err := store.WriteVersion(path, 0, []byte("# x\n"), nil); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		waitIdle(t, store)
	}
	for index := range 3 {
		write(fmt.Sprintf("/alone-%d.md", index))
	}
	if n := yields.Load(); n != 0 {
		t.Fatalf("a lone store yielded %d times", n)
	}
	if _, err := site.open(t, 0).WriteVersion("/peer.md", 0, []byte("# peer\n"), nil); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	write("/seen-1.md")
	write("/seen-2.md")
	if n := yields.Load(); n != 2 {
		t.Fatalf("yields after seeing a peer = %d, want one per win", n)
	}
	clock = clock.Add(yieldWindow)
	write("/later.md")
	if n := yields.Load(); n != 2 {
		t.Fatalf("yields once the peer is %v old = %d, want none", yieldWindow, n-2)
	}
}

// Writes read the document they build on while they wait in the queue, once
// for all those queued on one state, so their batch builds without reading the
// bucket again.
func TestQueuedWritesWarmTheirDocument(t *testing.T) {
	objects := initializedMemory(t)
	seeded, err := (&bucketSite{objects: objects}).open(t, 0).WriteVersion("/doc.md", 0, []byte("# Doc\n"), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	tip := blobKey(seeded.ETag)
	gets := newObservedBlobStore(objects)
	staged := &blobCreates{Store: gets, created: make(chan struct{}, 8)}
	holds := newSlotHolds(t, staged, 3)
	store := (&bucketSite{objects: holds, noHedge: true}).open(t, 0)
	read := func() int { return gets.counts().gets[tip] }
	opened := read() // the section index read the body at open
	var written atomic.Int64
	publish := func(path string) <-chan writeOutcome {
		body := fmt.Appendf(nil, "# %s, write %d\n", path, written.Add(1))
		return publishAsync(context.Background(), store, backend.WriteRequest{Path: path, ExpectedVersion: -1, Content: body})
	}
	ahead := fillPipeline(t, holds, staged, publish)
	updates := make([]<-chan writeOutcome, 2)
	for index := range updates {
		updates[index] = publish("/doc.md")
		waitQueued(t, store, index+1)
	}
	waitFor(t, "the queued writes' warm-up", func() bool { return read() > opened })
	holds.holds[3].open()
	for _, result := range append(ahead, updates...) {
		if outcome := <-result; outcome.err != nil {
			t.Fatalf("write: %v", outcome.err)
		}
	}
	if n := read() - opened; n != 1 {
		t.Errorf("the tip was read %d times after open, want once for both queued writes", n)
	}
}

// Open hedges its bucket: a create that stalls does not hold the write it
// belongs to until its deadline.
func TestStalledCreatesAreHedged(t *testing.T) {
	stalled := &stallFirstCreate{Store: initializedMemory(t), seen: make(map[string]bool)}
	store := (&bucketSite{objects: stalled}).open(t, 0)
	started := time.Now()
	if _, err := store.WriteVersion("/doc.md", 0, []byte("# Doc\n"), nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("write took %v with its blob and slot creates stalled, want them hedged", elapsed)
	}
	if n := stalled.stalls.Load(); n != 2 {
		t.Errorf("stalled creates = %d, want the blob's and the slot's", n)
	}
}

// stallFirstCreate holds the first create of every name until its context
// ends; later creates of the name go through.
type stallFirstCreate struct {
	blob.Store
	mu     sync.Mutex
	seen   map[string]bool
	stalls atomic.Int64
}

func (s *stallFirstCreate) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	s.mu.Lock()
	first := !s.seen[key]
	s.seen[key] = true
	s.mu.Unlock()
	if first {
		s.stalls.Add(1)
		<-ctx.Done()
		return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: ctx.Err()}
	}
	return s.Store.Create(ctx, key, data)
}

// The limit grows to a full slot while the queue stays deeper than a batch,
// and falls back once a batch is short.
func TestBatchLimit(t *testing.T) {
	store, _ := newWritableStore(t)
	c := &committer{store: store, limit: batchStart}
	queue := func(n int) {
		for range n {
			store.commits.waiting = append(store.commits.waiting, &commitRequest{ctx: context.Background(), answer: make(chan commitAnswer, 1)})
		}
	}
	queue(60)
	for _, want := range []int{batchStart, maxSlotEntries, 12} {
		if got := len(c.take()); got != want {
			t.Fatalf("batch of %d, want %d", got, want)
		}
	}
	if c.limit != batchStart {
		t.Fatalf("limit after a short batch = %d, want %d", c.limit, batchStart)
	}
}

// slotHold holds one slot create until released.
type slotHold struct {
	arrived chan struct{}
	release chan struct{}
	held    sync.Once
	opened  sync.Once
}

func newSlotHold() *slotHold {
	return &slotHold{arrived: make(chan struct{}), release: make(chan struct{})}
}

func (hold *slotHold) open() { hold.opened.Do(func() { close(hold.release) }) }

// slotHolds holds the first create of each named slot until it opens or the
// create's context ends; every hold opens when the test ends.
type slotHolds struct {
	blob.Store
	holds map[int64]*slotHold
}

func newSlotHolds(t *testing.T, objects blob.Store, slots ...int64) *slotHolds {
	holds := &slotHolds{Store: objects, holds: make(map[int64]*slotHold)}
	for _, first := range slots {
		holds.holds[first] = newSlotHold()
	}
	t.Cleanup(func() {
		for _, hold := range holds.holds {
			hold.open()
		}
	})
	return holds
}

func (s *slotHolds) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if first, ok := sequenceOfKey(key, logPrefix); ok {
		if hold := s.holds[first]; hold != nil {
			held := false
			hold.held.Do(func() { held = true })
			if held {
				close(hold.arrived)
				select {
				case <-hold.release:
				case <-ctx.Done():
					return blob.Attributes{}, &blob.OpError{Op: "create", Key: key, Err: ctx.Err()}
				}
			}
		}
	}
	return s.Store.Create(ctx, key, data)
}

// slowStore delays every call by up to 2 ms, so stores interleave.
type slowStore struct{ blob.Store }

func pause() { time.Sleep(time.Duration(mathrand.Int64N(int64(2 * time.Millisecond)))) }

func (s slowStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	pause()
	return s.Store.Create(ctx, key, data)
}

func (s slowStore) Get(ctx context.Context, key string) (blob.Object, error) {
	pause()
	return s.Store.Get(ctx, key)
}

func (s slowStore) List(ctx context.Context, prefix, startAfter, cursor string) (blob.ListResult, error) {
	pause()
	return s.Store.List(ctx, prefix, startAfter, cursor)
}

// raceCounter counts slot creates that found the name taken.
type raceCounter struct {
	blob.Store
	losses atomic.Int64
}

func (s *raceCounter) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	attributes, err := s.Store.Create(ctx, key, data)
	if isSlot(key) && errors.Is(err, blob.ErrPrecondition) {
		s.losses.Add(1)
	}
	return attributes, err
}

// logShape reads the log through tip: how many stores wrote it, and whether
// any slot holds more than one change.
func logShape(t *testing.T, objects blob.Store, tip int64) (writers int, batched bool) {
	t.Helper()
	stores := map[string]bool{}
	for first := int64(2); first <= tip; {
		read, err := readSlot(context.Background(), objects, testWorldID, first)
		if err != nil {
			t.Fatalf("read slot %d: %v", first, err)
		}
		stores[read.slot.Store] = true
		batched = batched || len(read.slot.Entries) > 1
		first = read.slot.last() + 1
	}
	return len(stores), batched
}

// fillPipeline makes /first.md's create of the first held slot hold and
// stages a batch for each of two writes behind it, so the next requests queue.
func fillPipeline(t *testing.T, holds *slotHolds, staged *blobCreates, write func(path string) <-chan writeOutcome) []<-chan writeOutcome {
	t.Helper()
	outcomes := make([]<-chan writeOutcome, 1, 3)
	outcomes[0] = write("/first.md")
	first := slices.Min(slices.Collect(maps.Keys(holds.holds)))
	waitForTestSignal(t, holds.holds[first].arrived, "the first held slot's create")
	<-staged.created
	for index := range 2 {
		outcomes = append(outcomes, write(fmt.Sprintf("/ahead-%d.md", index+1)))
		waitForTestSignal(t, staged.created, "a batch staged ahead")
	}
	return outcomes
}

// blobCreates signals every document blob it creates and counts attempts.
type blobCreates struct {
	blob.Store
	created  chan struct{}
	attempts atomic.Int64
}

func (s *blobCreates) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	attributes, err := s.Store.Create(ctx, key, data)
	if strings.HasPrefix(key, objectPrefix+"blobs/") {
		s.attempts.Add(1)
		if err == nil {
			s.created <- struct{}{}
		}
	}
	return attributes, err
}

// waitQueued waits until n requests wait in the queue.
func waitQueued(t *testing.T, store *Store, n int) {
	t.Helper()
	waitUntil(t, store, fmt.Sprintf("%d queued requests", n), func(queue *commitQueue) bool { return len(queue.waiting) == n })
}

// waitIdle waits until the committer has stopped.
func waitIdle(t *testing.T, store *Store) {
	t.Helper()
	waitUntil(t, store, "an idle committer", func(queue *commitQueue) bool { return !queue.running })
}

func waitUntil(t *testing.T, store *Store, name string, done func(*commitQueue) bool) {
	t.Helper()
	waitFor(t, name, func() bool {
		store.commits.mu.Lock()
		defer store.commits.mu.Unlock()
		return done(&store.commits)
	})
}

// waitFor polls reached until it holds, failing the test after five seconds.
func waitFor(t *testing.T, name string, reached func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !reached() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", name)
		}
		time.Sleep(time.Millisecond)
	}
}
