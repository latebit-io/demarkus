package bucketstore

import (
	"bytes"
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
	hinted := &hints{}
	store, err := Open(context.Background(), holds, Options{Logger: discardLogger, WorldID: testWorldID, noHedge: true, Committed: hinted.record})
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
	hinted.waitFor(t, 7)
	for index, sequence := range hinted.all() {
		if !slices.Contains([]int64{2, 3, 4, 5, 7}, sequence) || index > 0 && sequence <= hinted.all()[index-1] {
			t.Fatalf("hinted %v, want the last sequences of its slots and of the one it lost, in order", hinted.all())
		}
	}
}

// Committed reports a slot even when the write that ends it gave up waiting
// while the slot was being created.
func TestCommittedReportsSlotsItsWritersLeft(t *testing.T) {
	objects := initializedMemory(t)
	staged := &blobCreates{Store: objects, created: make(chan struct{}, 8)}
	holds := newSlotHolds(t, staged, 2, 5)
	hinted := &hints{}
	store, err := Open(context.Background(), holds, Options{Logger: discardLogger, WorldID: testWorldID, noHedge: true, Committed: hinted.record})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeAtEnd(t, store)
	write := func(ctx context.Context, path string) <-chan writeOutcome {
		return publishAsync(ctx, store, backend.WriteRequest{Path: path, ExpectedVersion: 0, Content: []byte("# " + path + "\n")})
	}
	ahead := fillPipeline(t, holds, staged, func(path string) <-chan writeOutcome { return write(context.Background(), path) })
	kept := write(context.Background(), "/kept.md")
	waitQueued(t, store, 1)
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	left := write(short, "/left.md")
	waitQueued(t, store, 2)
	holds.holds[2].open()
	waitForTestSignal(t, holds.holds[5].arrived, "slot 5's create")
	if outcome := <-left; !errors.Is(outcome.err, context.DeadlineExceeded) {
		t.Fatalf("write that gave up = %v, want its deadline", outcome.err)
	}
	holds.holds[5].open()
	for _, result := range append(ahead, kept) {
		if outcome := <-result; outcome.err != nil {
			t.Fatalf("write: %v", outcome.err)
		}
	}
	hinted.waitFor(t, 6)
}

// hints records the Committed hook's calls.
type hints struct {
	mu   sync.Mutex
	seen []int64
}

func (h *hints) record(sequence int64) {
	h.mu.Lock()
	h.seen = append(h.seen, sequence)
	h.mu.Unlock()
}

func (h *hints) all() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.seen)
}

// waitFor waits until the newest hint is sequence.
func (h *hints) waitFor(t *testing.T, sequence int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("a hint of sequence %d", sequence), func() bool {
		all := h.all()
		return len(all) > 0 && all[len(all)-1] == sequence
	})
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

// A store that loses a slot hints the winner, which reads it as a peer waiting.
// The loser does not hold: a commit hint that came during its own create is
// void once it installs the winner's slot.
func TestLosingStoreSignalsTheWinner(t *testing.T) {
	_, memory := newWritableStore(t)
	barrier := &createBarrierStore{Store: memory, arrived: make(chan struct{}, 2), release: make(chan struct{})}
	site := &bucketSite{objects: barrier, noHedge: true, hinted: true}
	left, right := site.open(t, 0), site.open(t, 0)
	results := runConcurrentWrites(left, "/left", right, "/right")
	barrier.releaseBoth(t)
	for _, outcome := range collectWriteOutcomes(t, results) {
		if outcome.err != nil {
			t.Fatalf("write: %v", outcome.err)
		}
	}
	slots := logSlots(t, memory, max(left.servedSequence(), right.servedSequence()))
	winner, loser := left, right
	if slots[len(slots)-2].Store == right.id {
		winner, loser = right, left
	}
	waitFor(t, "the winner to hear the loser", func() bool { return winner.waiting.Load() != nil })
	started := time.Now()
	if _, err := loser.WriteVersion("/again", 0, []byte("again"), nil); err != nil {
		t.Fatalf("loser's next write: %v", err)
	}
	if waited := time.Since(started); waited > loser.handoffWait/2 {
		t.Errorf("the loser's next write took %v: it held a slot for the winner", waited)
	}
}

// Follow tells a hint inside one of this store's newest slots, which only a
// peer that lost that slot sends, from a hint inside a peer's slot.
func TestFollowTellsLoserHintsApart(t *testing.T) {
	site := &bucketSite{objects: initializedMemory(t)}
	store, peer := site.open(t, 0), site.open(t, 0)
	write := func(writer *Store, path string) int64 {
		t.Helper()
		if _, err := writer.WriteVersion(path, 0, []byte("# x\n"), nil); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return writer.servedSequence()
	}
	own := write(store, "/own.md")
	theirs := write(peer, "/theirs.md")
	if _, err := store.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	store.Follow(theirs)
	if store.waiting.Load() != nil {
		t.Fatal("a hint inside a peer's slot read as a peer waiting")
	}
	// A peer pipelining small slots may write dozens before the hint arrives.
	for index := range 64 {
		write(store, fmt.Sprintf("/since-%d.md", index))
	}
	store.Follow(own)
	if store.waiting.Load() == nil {
		t.Fatal("a hint inside this store's slot 64 slots back was not read as a peer waiting")
	}
	store.waiting.Store(nil)
	// A loser can hear of a slot before its writer has seen its create succeed.
	tip := store.servedSequence()
	store.pending.Store(&pendingSlot{base: &snapshot{Sequence: tip}, next: &snapshot{Sequence: tip + 2}})
	store.Follow(tip + 1)
	store.pending.Store(nil)
	if store.waiting.Load() == nil {
		t.Fatal("a hint inside the slot being created was not read as a peer waiting")
	}
	store.waiting.Store(nil)
	for index := range ownSlots {
		write(store, fmt.Sprintf("/later-%d.md", index))
	}
	store.Follow(own)
	if store.waiting.Load() != nil {
		t.Errorf("a hint for a slot %d slots back still read as waiting", ownSlots)
	}
}

// After a win with a peer waiting, the next slot is held until the peer's
// slot is installed, then rebuilt on it without losing a create; a peer that
// never comes costs at most handoffWait.
func TestWinnerHandsTheNextSlotToAWaitingPeer(t *testing.T) {
	setup := func(t *testing.T) (*Store, *Store, *raceCounter) {
		objects := &raceCounter{Store: initializedMemory(t)}
		site := &bucketSite{objects: objects}
		store, peer := site.open(t, 16), site.open(t, 0)
		if _, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil); err != nil {
			t.Fatalf("write /a.md: %v", err)
		}
		store.Follow(store.servedSequence())
		if _, err := store.WriteVersion("/b.md", 0, []byte("# b\n"), nil); err != nil {
			t.Fatalf("write /b.md: %v", err)
		}
		return store, peer, objects
	}
	held := func(store *Store) (<-chan error, time.Time) {
		done := make(chan error, 1)
		started := time.Now()
		go func() {
			_, err := store.WriteVersion("/c.md", 0, []byte("# c\n"), nil)
			done <- err
		}()
		return done, started
	}

	t.Run("until the peer's slot", func(t *testing.T) {
		store, peer, objects := setup(t)
		done, _ := held(store)
		select {
		case err := <-done:
			t.Fatalf("the next slot committed during the handoff: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		if _, err := peer.WriteVersion("/p.md", 0, []byte("# p\n"), nil); err != nil {
			t.Fatalf("peer write: %v", err)
		}
		peerSequence := peer.servedSequence()
		store.Follow(peerSequence)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("held write: %v", err)
			}
		case <-time.After(store.handoffWait / 2):
			t.Fatal("the held write did not commit once the peer's slot was installed")
		}
		after, err := readSlot(context.Background(), objects, testWorldID, peerSequence+1)
		if err != nil || after.slot.Store != store.id {
			t.Fatalf("slot after the peer's = (%+v, %v), want this store's", after.slot, err)
		}
		if n := objects.losses.Load(); n != 0 {
			t.Errorf("lost slot creates = %d, want the held batch rebuilt before its create", n)
		}
	})

	t.Run("not for a stale hint", func(t *testing.T) {
		store := (&bucketSite{objects: initializedMemory(t)}).open(t, 16)
		store.handoffWait = 100 * time.Millisecond
		if _, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil); err != nil {
			t.Fatalf("write /a.md: %v", err)
		}
		store.Follow(store.servedSequence())
		time.Sleep(store.handoffWait + 20*time.Millisecond)
		if _, err := store.WriteVersion("/b.md", 0, []byte("# b\n"), nil); err != nil {
			t.Fatalf("write /b.md: %v", err)
		}
		done, started := held(store)
		if err := <-done; err != nil {
			t.Fatalf("write /c.md: %v", err)
		}
		if waited := time.Since(started); waited >= store.handoffWait/2 {
			t.Errorf("write after a stale hint took %v, want no hold", waited)
		}
	})

	t.Run("at most handoffWait", func(t *testing.T) {
		store := (&bucketSite{objects: initializedMemory(t)}).open(t, 16)
		store.handoffWait = 100 * time.Millisecond
		if _, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil); err != nil {
			t.Fatalf("write /a.md: %v", err)
		}
		store.Follow(store.servedSequence())
		if _, err := store.WriteVersion("/b.md", 0, []byte("# b\n"), nil); err != nil {
			t.Fatalf("write /b.md: %v", err)
		}
		done, started := held(store)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("held write: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a hold for a peer that never came did not end")
		}
		if waited := time.Since(started); waited < 50*time.Millisecond {
			t.Errorf("held write committed after %v, want it held", waited)
		}
	})
}

// Two stores writing one world as fast as they can both keep committing:
// the one that loses races hints the winner, which hands it the next slot.
func TestContendingStoresTakeTurns(t *testing.T) {
	site := &bucketSite{objects: evenLatency(initializedMemory(t), 5*time.Millisecond), hinted: true}
	stores := []*Store{site.open(t, 16), site.open(t, 16)}
	deadline := time.Now().Add(1500 * time.Millisecond)
	var committed [2]atomic.Int64
	var wg sync.WaitGroup
	for index, store := range stores {
		for writer := range 8 {
			wg.Go(func() {
				for n := 0; time.Now().Before(deadline); n++ {
					path := fmt.Sprintf("/s%d/w%d/%d.md", index, writer, n)
					if _, err := store.WriteVersion(path, 0, []byte("# x\n"), nil); err != nil {
						t.Errorf("write %s: %v", path, err)
						return
					}
					if time.Now().Before(deadline) {
						committed[index].Add(1)
					}
				}
			})
		}
	}
	wg.Wait()
	left, right := committed[0].Load(), committed[1].Load()
	if left+right == 0 {
		t.Fatal("no write committed")
	}
	t.Logf("writes committed during the run: %d and %d", left, right)
	if least := min(left, right); least*3 < left+right {
		t.Errorf("writes committed during the run: %d and %d; one store starved", left, right)
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

// The batches behind a lost slot are rebuilt from what they had read: a
// requeued update does not read its document from the bucket again, so a
// loser creates again within a few bucket calls rather than one per member.
func TestDiscardedBatchesKeepTheirReads(t *testing.T) {
	objects := initializedMemory(t)
	seeder := (&bucketSite{objects: objects}).open(t, 0)
	tips := make(map[string]string)
	for _, path := range []string{"/first.md", "/ahead-1.md", "/ahead-2.md"} {
		seeded, err := seeder.WriteVersion(path, 0, []byte("# "+path+"\n"), nil)
		if err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		tips[path] = blobKey(seeded.ETag)
	}
	gets := newObservedBlobStore(objects)
	staged := &blobCreates{Store: gets, created: make(chan struct{}, 8)}
	holds := newSlotHolds(t, staged, 5)
	store := (&bucketSite{objects: holds, noHedge: true}).open(t, 0)
	update := func(path string) <-chan writeOutcome {
		return publishAsync(context.Background(), store, backend.WriteRequest{Path: path, ExpectedVersion: -1, Content: []byte("# again\n")})
	}
	ahead := fillPipeline(t, holds, staged, update)
	read := func(path string) int { return gets.counts().gets[tips[path]] }
	before := map[string]int{"/ahead-1.md": read("/ahead-1.md"), "/ahead-2.md": read("/ahead-2.md")}

	// A peer takes slot 5: the head rebuilds and the staged batches requeue.
	if _, err := (&bucketSite{objects: objects}).open(t, 0).WriteVersion("/peer.md", 0, []byte("# peer\n"), nil); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	holds.holds[5].open()
	for index, result := range ahead {
		if outcome := <-result; outcome.err != nil || outcome.document.Version != 2 {
			t.Fatalf("write %d = %+v, %v; want v2", index, outcome.document, outcome.err)
		}
	}
	for path, n := range before {
		if again := read(path) - n; again != 0 {
			t.Errorf("%s's tip was read %d times more after the lost slot, want its batch's read kept", path, again)
		}
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

// latentStore delays each kind of call by a fixed delay plus up to a tenth
// of jitter, as a bucket's latency does.
type latentStore struct {
	blob.Store
	create, get, list time.Duration
}

// evenLatency delays every call by delay.
func evenLatency(objects blob.Store, delay time.Duration) latentStore {
	return latentStore{Store: objects, create: delay, get: delay, list: delay}
}

// gcsLatency is what the GCS gate measured per call (research, 2026-10-04).
func gcsLatency(objects blob.Store) latentStore {
	return latentStore{Store: objects, create: 45 * time.Millisecond, get: 25 * time.Millisecond, list: 30 * time.Millisecond}
}

func (s latentStore) wait(delay time.Duration) {
	if delay > 0 {
		time.Sleep(delay + jitterWithin(max(delay/10, time.Nanosecond)))
	}
}

func (s latentStore) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	s.wait(s.create)
	return s.Store.Create(ctx, key, data)
}

func (s latentStore) Get(ctx context.Context, key string) (blob.Object, error) {
	s.wait(s.get)
	return s.Store.Get(ctx, key)
}

func (s latentStore) List(ctx context.Context, prefix, startAfter, cursor string) (blob.ListResult, error) {
	s.wait(s.list)
	return s.Store.List(ctx, prefix, startAfter, cursor)
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
	for _, slot := range logSlots(t, objects, tip) {
		stores[slot.Store] = true
		batched = batched || len(slot.Entries) > 1
	}
	return len(stores), batched
}

// logSlots reads the log's slots through tip, in order.
func logSlots(t *testing.T, objects blob.Store, tip int64) []*slotObject {
	t.Helper()
	var slots []*slotObject
	for first := int64(2); first <= tip; {
		read, err := readSlot(context.Background(), objects, testWorldID, first)
		if err != nil {
			t.Fatalf("read slot %d: %v", first, err)
		}
		slots = append(slots, read.slot)
		first = read.slot.last() + 1
	}
	return slots
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

// writersModel is R stores on one bucket with GCS-shaped latency under a
// closed or an open load: the in-process model of several replicas writing
// one world (plan: several-writers).
type writersModel struct {
	replicas    int
	handoffWait time.Duration
	hinted      bool
	// closed is the writers per store looping as fast as they are answered;
	// zero for an open loop offering rate writes/s over all stores.
	closed   int
	rate     int
	duration time.Duration
}

// modelSeed is the documents a model world holds before the load.
const modelSeed = 2000

// modelResult is what one run shows: the acknowledged writes, the log's
// shape, and what the slot creates cost each store.
type modelResult struct {
	acked, failed int64
	// offered is how many writes the load started: a ticker drops ticks
	// when its receiver lags, so the open loop offers fewer than its rate.
	offered  int64
	perStore []int64
	// acks are the acknowledged (path, version) pairs with how many slot
	// entries hold each; every one must be exactly one.
	acks    map[string]int
	p50     time.Duration
	slots   int
	entries int
	losses  int64
	// recoveryP50 is a loss to the same store's next create; afterWinP50 a
	// win to its next create, and holds counts those over 200 ms.
	recoveryP50, afterWinP50 time.Duration
	holds                    int
}

func (r modelResult) String() string {
	return fmt.Sprintf("acked %d (%v) failed %d p50 %v | slots %d entries/slot %.1f losses %d | recovery p50 %v after-win p50 %v holds %d",
		r.acked, r.perStore, r.failed, r.p50.Round(time.Millisecond), r.slots, float64(r.entries)/float64(max(r.slots, 1)), r.losses,
		r.recoveryP50.Round(time.Millisecond), r.afterWinP50.Round(time.Millisecond), r.holds)
}

// slotAttempt is one slot create a store made.
type slotAttempt struct {
	started, ended time.Time
	lost           bool
}

// slotProbe records each slot create of the store it serves.
type slotProbe struct {
	blob.Store
	mu       sync.Mutex
	attempts []slotAttempt
}

func (p *slotProbe) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	if !isSlot(key) {
		return p.Store.Create(ctx, key, data)
	}
	started := time.Now()
	attributes, err := p.Store.Create(ctx, key, data)
	p.mu.Lock()
	p.attempts = append(p.attempts, slotAttempt{started: started, ended: time.Now(), lost: errors.Is(err, blob.ErrPrecondition)})
	p.mu.Unlock()
	return attributes, err
}

// gaps are the time from each attempt's end to the store's next create,
// split by whether the attempt lost.
func (p *slotProbe) gaps() (recovery, afterWin []time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for index, attempt := range p.attempts[:max(len(p.attempts)-1, 0)] {
		gap := p.attempts[index+1].started.Sub(attempt.ended)
		if attempt.lost {
			recovery = append(recovery, gap)
		} else {
			afterWin = append(afterWin, gap)
		}
	}
	return recovery, afterWin
}

func percentile(samples []time.Duration, fraction float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[min(int(float64(len(sorted))*fraction), len(sorted)-1)]
}

// run opens the stores on one bucket, drives the load for the duration and
// reads the log back without latency.
func (m writersModel) run(t *testing.T) modelResult {
	t.Helper()
	memory := initializedMemory(t)
	// The seed is written without latency; the load's builds then read it
	// through the bucket, as the gate's updates of random documents did.
	writeDocuments(t, (&bucketSite{objects: memory}).open(t, 0), modelSeed)
	bucket := gcsLatency(memory)
	site := &bucketSite{hinted: m.hinted, noHedge: true}
	stores := make([]*Store, m.replicas)
	probes := make([]*slotProbe, m.replicas)
	for index := range stores {
		probes[index] = &slotProbe{Store: bucket}
		stores[index] = site.openOn(t, probes[index], 16)
		stores[index].handoffWait = m.handoffWait
	}
	body := bytes.Repeat([]byte("# model\n"), 128)
	var acked, failed atomic.Int64
	perStore := make([]atomic.Int64, m.replicas)
	var latencies struct {
		mu      sync.Mutex
		samples []time.Duration
		acks    map[string]int
	}
	latencies.acks = make(map[string]int)
	var failure sync.Once
	deadline := time.Now().Add(m.duration)
	write := func(index int, n int) {
		path, expected := fmt.Sprintf("/new/%d.md", n), 0
		if n%10 >= 3 {
			// Seven writes in ten update a random document blind (expected -1).
			path, expected = fmt.Sprintf("/many/%04d.md", mathrand.IntN(modelSeed)), -1
		}
		started := time.Now()
		document, err := stores[index].WriteVersion(path, expected, fmt.Appendf(body, "write %d\n", n), nil)
		if err != nil {
			failed.Add(1)
			failure.Do(func() { t.Logf("write %s: %v", path, err) })
			return
		}
		latencies.mu.Lock()
		latencies.acks[fmt.Sprintf("%s@%d", path, document.Version)] = 0
		latencies.mu.Unlock()
		if m.closed > 0 && !started.Before(deadline) {
			return
		}
		acked.Add(1)
		perStore[index].Add(1)
		latencies.mu.Lock()
		latencies.samples = append(latencies.samples, time.Since(started))
		latencies.mu.Unlock()
	}
	var wg sync.WaitGroup
	var sequence atomic.Int64
	if m.closed > 0 {
		for index := range stores {
			for range m.closed {
				wg.Go(func() {
					for time.Now().Before(deadline) {
						write(index, int(sequence.Add(1)))
					}
				})
			}
		}
	} else {
		ticker := time.NewTicker(time.Second / time.Duration(m.rate))
		for time.Now().Before(deadline) {
			<-ticker.C
			n := int(sequence.Add(1))
			wg.Go(func() { write(n%m.replicas, n) })
		}
		ticker.Stop()
	}
	wg.Wait()

	result := modelResult{acked: acked.Load(), failed: failed.Load(), offered: sequence.Load(), p50: percentile(latencies.samples, 0.5), acks: latencies.acks}
	for index := range perStore {
		result.perStore = append(result.perStore, perStore[index].Load())
	}
	var tip int64
	for _, store := range stores {
		tip = max(tip, store.servedSequence())
	}
	for _, slot := range logSlots(t, memory, tip) {
		result.slots++
		result.entries += len(slot.Entries)
		for index := range slot.Entries {
			key := fmt.Sprintf("%s@%d", slot.Entries[index].Path, slot.Entries[index].Current)
			if _, acknowledged := result.acks[key]; acknowledged {
				result.acks[key]++
			}
		}
	}
	recovery, afterWin := make([]time.Duration, 0, result.slots), make([]time.Duration, 0, result.slots)
	for _, probe := range probes {
		lost, won := probe.gaps()
		recovery, afterWin = append(recovery, lost...), append(afterWin, won...)
		probe.mu.Lock()
		for _, attempt := range probe.attempts {
			if attempt.lost {
				result.losses++
			}
		}
		probe.mu.Unlock()
	}
	result.recoveryP50, result.afterWinP50 = percentile(recovery, 0.5), percentile(afterWin, 0.5)
	for _, gap := range afterWin {
		if gap > 200*time.Millisecond {
			result.holds++
		}
	}
	return result
}

// Several stores writing one world on GCS-shaped latency: the model of the
// GCS gate's three-writer collapse (94 writes/s against 365 with one) and
// the regression test for its fix; the guarantees it holds are in check.
func TestSeveralWritersModel(t *testing.T) {
	if testing.Short() {
		t.Skip("the several-writers model runs for about 20 s")
	}
	const duration, rate = 3 * time.Second, 300
	// No write fails, every acknowledged write is in exactly one slot, no
	// store starves, and a loser creates again within a few bucket calls.
	check := func(t *testing.T, name string, result modelResult) {
		t.Helper()
		if result.failed > 0 {
			t.Errorf("%s: %d writes failed", name, result.failed)
		}
		for key, n := range result.acks {
			if n != 1 {
				t.Errorf("%s: acknowledged %s is in %d slots", name, key, n)
			}
		}
		// A store that keeps losing acknowledges its first batch and no more.
		if least := slices.Min(result.perStore); least*2*int64(len(result.perStore)) < result.acked {
			t.Errorf("%s: a store acknowledged %d of %d writes; it starved", name, least, result.acked)
		}
		if result.recoveryP50 > 500*time.Millisecond {
			t.Errorf("%s: a loser took %v to create again, want a few bucket calls", name, result.recoveryP50)
		}
	}
	var alone float64
	for _, replicas := range []int{1, 2, 3} {
		closed := writersModel{replicas: replicas, handoffWait: handoffWait, hinted: true, closed: 64, duration: duration}.run(t)
		perSecond := float64(closed.acked) / duration.Seconds()
		t.Logf("R=%d closed 64/store: %.0f writes/s; %v", replicas, perSecond, closed)
		check(t, fmt.Sprintf("R=%d closed", replicas), closed)
		if replicas == 1 {
			alone = perSecond
		} else if perSecond < 0.7*alone {
			t.Errorf("R=%d closed loop: %.0f writes/s, under 70%% of one writer's %.0f", replicas, perSecond, alone)
		}
		open := writersModel{replicas: replicas, handoffWait: handoffWait, hinted: true, rate: rate, duration: duration}.run(t)
		t.Logf("R=%d open %d/s:       %.0f writes/s; %v", replicas, rate, float64(open.acked)/duration.Seconds(), open)
		check(t, fmt.Sprintf("R=%d open", replicas), open)
		if open.acked < open.offered*9/10 || open.p50 > 1500*time.Millisecond {
			t.Errorf("R=%d open loop: %d of %d offered writes acknowledged at p50 %v", replicas, open.acked, open.offered, open.p50)
		}
	}
}
