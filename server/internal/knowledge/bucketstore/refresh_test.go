package bucketstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// A read by path fails on its own deadline while another caller's probe of
// the log is stalled on the bucket: it waits on the probe no longer than its
// context (plan: refresh-flight).
func TestReadFailsOnItsOwnDeadlineWhileAProbeStalls(t *testing.T) {
	hold := newGetHold(t, initializedMemory(t), slotKey(3))
	store := (&bucketSite{objects: hold, noHedge: true}).open(t, 0)
	_, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil)
	mustSucceed(t, err)

	stalled := readAsync(context.Background(), store, "/a.md")
	waitForTestSignal(t, hold.arrived, "the probe of slot 3")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	select {
	case err := <-readAsync(ctx, store, "/a.md"):
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("read behind a stalled probe = %v, want its own deadline", err)
		}
		if waited := time.Since(started); waited > time.Second {
			t.Errorf("the read gave up after %v, want its 200 ms deadline", waited)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the read waited on another caller's probe past its own deadline")
	}
	hold.open()
	mustSucceed(t, <-stalled)
}

// A store whose slot create lost while a read's probe is stalled on the
// bucket catches up through the winner and creates again within a few bucket
// calls: a loser's recovery never waits behind a probe (plan: refresh-flight).
func TestLoserCreatesAgainWhileAProbeStalls(t *testing.T) {
	memory := initializedMemory(t)
	calls := &bucketCalls{Store: memory}
	hold := newGetHold(t, calls, slotKey(3))
	hold.armed.Store(false) // the committer's own probe of slot 3 goes through
	holds := newSlotHolds(t, hold, 3)
	site := &bucketSite{noHedge: true}
	store, peer := site.openOn(t, holds, 0), site.openOn(t, memory, 0)
	_, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil)
	mustSucceed(t, err)

	// The create of slot 3 is held, then a read's probe of slot 3 stalls.
	written := publishAsync(context.Background(), store, backend.WriteRequest{Path: "/b.md", ExpectedVersion: 0, Content: []byte("# b\n")})
	waitForTestSignal(t, holds.holds[3].arrived, "the create of slot 3")
	hold.armed.Store(true)
	stalled := readAsync(context.Background(), store, "/a.md")
	waitForTestSignal(t, hold.arrived, "the probe of slot 3")
	// The peer takes slot 3, so the held create loses once it goes through.
	_, err = peer.WriteVersion("/p.md", 0, []byte("# p\n"), nil)
	mustSucceed(t, err)
	holds.holds[3].open()
	select {
	case outcome := <-written:
		mustSucceed(t, outcome.err)
	case <-time.After(2 * time.Second):
		t.Fatal("the loser's recovery waited on a read's probe stalled on the bucket")
	}
	hold.open()
	mustSucceed(t, <-stalled)

	between := calls.between(slotKey(3), slotKey(4))
	if len(between) > 4 {
		t.Errorf("the loser made %d bucket calls between its lost create and its next: %v", len(between), between)
	}
	if got := store.servedSequence(); got != 4 {
		t.Errorf("served sequence %d after the loss, the recovery and the read, want 4", got)
	}
	readEveryVersion(t, store)
}

// Refreshes share one probe: a caller that asks while a probe is in flight
// takes its tip when the probe confirms it after the ask, without a second
// probe; one that asks after the probe confirmed probes again.
func TestRefreshesShareOneProbe(t *testing.T) {
	t.Run("confirmed after the ask", func(t *testing.T) {
		memory := initializedMemory(t)
		calls := &bucketCalls{Store: memory}
		hold := newGetHold(t, calls, slotKey(3))
		site := &bucketSite{noHedge: true}
		store, peer := site.openOn(t, hold, 0), site.openOn(t, memory, 0)
		clock := countClock(store)
		_, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil)
		mustSucceed(t, err)
		_, err = peer.WriteVersion("/p.md", 0, []byte("# p\n"), nil)
		mustSucceed(t, err)
		calls.reset()

		first := refreshAsync(store)
		waitForTestSignal(t, hold.arrived, "the probe of slot 3")
		second := askWhileProbing(t, store, clock)
		hold.open()
		for _, outcome := range []<-chan refreshOutcome{first, second} {
			got := <-outcome
			mustSucceed(t, got.err)
			if got.sequence != 3 {
				t.Errorf("refresh returned sequence %d, want 3", got.sequence)
			}
		}
		// The slot's listing confirmed the tip after the second ask.
		if gets, lists := calls.slotGets(), calls.lists(); gets != 1 || lists != 1 {
			t.Errorf("two refreshes made %d slot gets and %d listings, want one probe", gets, lists)
		}
	})

	t.Run("confirmed before the ask", func(t *testing.T) {
		calls := &bucketCalls{Store: initializedMemory(t)}
		hold := newGetHold(t, calls, slotKey(3))
		store := (&bucketSite{objects: hold, noHedge: true}).open(t, 0)
		clock := countClock(store)
		_, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil)
		mustSucceed(t, err)
		calls.reset()

		first := refreshAsync(store)
		waitForTestSignal(t, hold.arrived, "the probe of slot 3")
		second := askWhileProbing(t, store, clock)
		hold.open()
		for _, outcome := range []<-chan refreshOutcome{first, second} {
			got := <-outcome
			mustSucceed(t, got.err)
			if got.sequence != 2 {
				t.Errorf("refresh returned sequence %d, want 2", got.sequence)
			}
		}
		// A missing slot confirms the tip as of the probe's start, before the
		// second ask, so the second caller probes again.
		if gets := calls.slotGets(); gets != 2 {
			t.Errorf("two refreshes made %d slot gets, want the second to probe again", gets)
		}
	})
}

// A probe that fails on the bucket answers every refresh waiting on it with
// that error, so a persistent failure costs one probe per burst; a probe that
// ended because its starter's context did answers none, and a waiter probes.
func TestRefreshesShareAProbeFailure(t *testing.T) {
	t.Run("a bucket error reaches every waiter", func(t *testing.T) {
		calls := &bucketCalls{Store: initializedMemory(t)}
		hold := newGetHold(t, calls, slotKey(3))
		hold.failWith = blob.ErrUnavailable
		store := (&bucketSite{objects: hold, noHedge: true}).open(t, 0)
		clock := countClock(store)
		_, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil)
		mustSucceed(t, err)
		calls.reset()

		first := refreshAsync(store)
		waitForTestSignal(t, hold.arrived, "the probe of slot 3")
		second := askWhileProbing(t, store, clock)
		hold.open()
		for _, outcome := range []<-chan refreshOutcome{first, second} {
			if got := <-outcome; !errors.Is(got.err, blob.ErrUnavailable) {
				t.Errorf("refresh behind a failed probe = %v, want the bucket's error", got.err)
			}
		}
		// The failed probe never reached the bucket behind the hold; a second
		// probe would have.
		if gets := calls.slotGets(); gets != 0 {
			t.Errorf("%d slot gets reached the bucket after a probe failed, want no second probe", gets)
		}
	})

	t.Run("a canceled starter does not fail a waiter", func(t *testing.T) {
		calls := &bucketCalls{Store: initializedMemory(t)}
		hold := newGetHold(t, calls, slotKey(3))
		store := (&bucketSite{objects: hold, noHedge: true}).open(t, 0)
		clock := countClock(store)
		_, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil)
		mustSucceed(t, err)
		calls.reset()

		ctx, cancel := context.WithCancel(context.Background())
		first := refreshWith(ctx, store)
		waitForTestSignal(t, hold.arrived, "the probe of slot 3")
		second := askWhileProbing(t, store, clock)
		cancel()
		if got := <-first; !errors.Is(got.err, context.Canceled) {
			t.Errorf("the canceled starter's refresh = %v, want its own cancellation", got.err)
		}
		got := <-second
		mustSucceed(t, got.err)
		if got.sequence != 2 {
			t.Errorf("the waiter's refresh returned sequence %d, want 2", got.sequence)
		}
		// The canceled probe never reached the bucket behind the hold.
		if gets := calls.slotGets(); gets != 1 {
			t.Errorf("%d slot gets reached the bucket, want the waiter's own probe", gets)
		}
	})
}

// A loser whose served snapshot already passed the winner's slot, installed by
// a probe meanwhile, rebuilds on that snapshot without joining the probe in
// flight, which may be stalled: its recovery stays out of the flight.
func TestLoserRecoversWithoutJoiningAStalledProbe(t *testing.T) {
	memory := initializedMemory(t)
	calls := &bucketCalls{Store: memory}
	hold := newGetHold(t, calls, slotKey(4))
	hold.armed.Store(false)
	holds := newSlotHolds(t, hold, 3)
	site := &bucketSite{noHedge: true}
	store, peer := site.openOn(t, holds, 0), site.openOn(t, memory, 0)
	_, err := store.WriteVersion("/a.md", 0, []byte("# a\n"), nil)
	mustSucceed(t, err)

	// The create of slot 3 is held; the peer takes slot 3, and a read's probe
	// installs it here, so served is past the winner before the loss is seen.
	written := publishAsync(context.Background(), store, backend.WriteRequest{Path: "/b.md", ExpectedVersion: 0, Content: []byte("# b\n")})
	waitForTestSignal(t, holds.holds[3].arrived, "the create of slot 3")
	_, err = peer.WriteVersion("/p.md", 0, []byte("# p\n"), nil)
	mustSucceed(t, err)
	mustSucceed(t, <-readAsync(context.Background(), store, "/a.md"))
	if got := store.servedSequence(); got != 3 {
		t.Fatalf("served sequence %d after the read, want the peer's slot 3", got)
	}
	// Then a probe of slot 4 stalls, and the held create goes through and loses.
	hold.armed.Store(true)
	stalled := readAsync(context.Background(), store, "/a.md")
	waitForTestSignal(t, hold.arrived, "the probe of slot 4")
	holds.holds[3].open()
	select {
	case outcome := <-written:
		mustSucceed(t, outcome.err)
	case <-time.After(2 * time.Second):
		t.Fatal("the loser's recovery joined a probe stalled on the bucket")
	}
	hold.open()
	mustSucceed(t, <-stalled)
	if between := calls.between(slotKey(3), slotKey(4)); len(between) > 2 {
		t.Errorf("the loser made %d bucket calls between its lost create and its next: %v", len(between), between)
	}
	if got := store.servedSequence(); got != 4 {
		t.Errorf("served sequence %d after the recovery and the read, want 4", got)
	}
	readEveryVersion(t, store)
}

// readAsync reads the current version of path on a view opened with ctx.
func readAsync(ctx context.Context, store *Store, path string) <-chan error {
	done := make(chan error, 1)
	go func() {
		view, err := store.OpenReadView(ctx)
		if err != nil {
			done <- err
			return
		}
		defer view.Close() //nolint:errcheck // a snapshot view's close cannot fail
		_, err = view.Get(ctx, path, 0)
		done <- err
	}()
	return done
}

type refreshOutcome struct {
	sequence int64
	err      error
}

func refreshAsync(store *Store) <-chan refreshOutcome {
	return refreshWith(context.Background(), store)
}

func refreshWith(ctx context.Context, store *Store) <-chan refreshOutcome {
	done := make(chan refreshOutcome, 1)
	go func() {
		snap, err := store.refresh(ctx)
		outcome := refreshOutcome{err: err}
		if snap != nil {
			outcome.sequence = snap.Sequence
		}
		done <- outcome
	}()
	return done
}

// countClock counts the store's clock reads; set before anything runs on
// the store, so a test can tell when a refresh has taken its asked time.
func countClock(store *Store) *atomic.Int64 {
	var reads atomic.Int64
	store.now = func() time.Time {
		reads.Add(1)
		return time.Now()
	}
	return &reads
}

// askWhileProbing starts a refresh and returns once it has asked; with the
// probe stalled and the store otherwise idle, the next clock read is its ask.
func askWhileProbing(t *testing.T, store *Store, clock *atomic.Int64) <-chan refreshOutcome {
	t.Helper()
	before := clock.Load()
	outcome := refreshAsync(store)
	waitFor(t, "the second refresh to ask", func() bool { return clock.Load() > before })
	return outcome
}

// getHold holds the first read of one key while armed until opened or the
// read's context ends; later reads of the key go through. It opens when the
// test ends.
type getHold struct {
	blob.Store
	key     string
	armed   atomic.Bool
	arrived chan struct{}
	release chan struct{}
	first   sync.Once
	opened  sync.Once
	// failWith, when set, is what the held read answers once released.
	failWith error
}

func newGetHold(t *testing.T, objects blob.Store, key string) *getHold {
	hold := &getHold{Store: objects, key: key, arrived: make(chan struct{}), release: make(chan struct{})}
	hold.armed.Store(true)
	t.Cleanup(hold.open)
	return hold
}

func (hold *getHold) open() { hold.opened.Do(func() { close(hold.release) }) }

func (hold *getHold) Get(ctx context.Context, key string) (blob.Object, error) {
	if key == hold.key && hold.armed.Load() {
		first := false
		hold.first.Do(func() { first = true })
		if first {
			close(hold.arrived)
			select {
			case <-hold.release:
				if hold.failWith != nil {
					return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: hold.failWith}
				}
			case <-ctx.Done():
				return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: ctx.Err()}
			}
		}
	}
	return hold.Store.Get(ctx, key)
}

// bucketCalls records every create, get and listing in order.
type bucketCalls struct {
	blob.Store
	mu    sync.Mutex
	calls []bucketCall
}

type bucketCall struct{ op, key string }

func (s *bucketCalls) record(op, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, bucketCall{op: op, key: key})
}

func (s *bucketCalls) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

func (s *bucketCalls) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	s.record("create", key)
	return s.Store.Create(ctx, key, data)
}

func (s *bucketCalls) Get(ctx context.Context, key string) (blob.Object, error) {
	s.record("get", key)
	return s.Store.Get(ctx, key)
}

func (s *bucketCalls) List(ctx context.Context, prefix, startAfter, cursor string) (blob.ListResult, error) {
	s.record("list", prefix)
	return s.Store.List(ctx, prefix, startAfter, cursor)
}

func (s *bucketCalls) slotGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, call := range s.calls {
		if call.op == "get" && isSlot(call.key) {
			n++
		}
	}
	return n
}

func (s *bucketCalls) lists() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, call := range s.calls {
		if call.op == "list" {
			n++
		}
	}
	return n
}

// between is the calls after the last create of from and before the first
// create of to.
func (s *bucketCalls) between(from, to string) []bucketCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	start, end := -1, len(s.calls)
	for index, call := range s.calls {
		if call.op != "create" {
			continue
		}
		switch {
		case call.key == from:
			start = index
		case call.key == to && index > start && end == len(s.calls):
			end = index
		}
	}
	if start < 0 {
		return nil
	}
	return s.calls[start+1 : end]
}
