package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

func waitServed(t *testing.T, store *Store, sequence int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("served sequence %d", sequence), func() bool { return store.servedSequence() >= sequence })
}

// The backstop reads the bucket only while a watcher waits on the hub: an
// open subscription nobody reads does not count.
func TestFollowerPollsOnlyForWaitingWatchers(t *testing.T) {
	site := &bucketSite{objects: initializedMemory(t), follow: 10 * time.Millisecond}
	store := site.open(t, changefeed.DefaultRingSize)
	sub, err := store.Changes().Subscribe(t.Context(), "/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	before := store.servedSequence()
	site.Tamper(t, "/docs/peer.md")

	time.Sleep(100 * time.Millisecond) // ten backstop periods
	if got := store.servedSequence(); got != before {
		t.Fatalf("served sequence moved to %d with nobody reading", got)
	}
	if ev := storetest.NextEvent(t, sub); ev.Path != "/docs/peer.md" {
		t.Fatalf("watcher saw %s, want the peer's commit", ev.Path)
	}
}

// A peer's hint reads at once, long before the default backstop.
func TestFollowReadsAtOnce(t *testing.T) {
	objects := initializedMemory(t)
	site := &bucketSite{objects: objects}
	store := site.open(t, changefeed.DefaultRingSize)
	site.Tamper(t, "/docs/hinted.md")
	tip := site.open(t, 0).servedSequence()
	store.Follow(tip)
	waitServed(t, store, tip)
}

// Close stops the follow loop, ends the hub's watches and refuses writes;
// a later hint and a second Close are no-ops.
func TestCloseStopsFollowing(t *testing.T) {
	r := openReplica(t, initializedMemory(t))
	if err := r.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-r.store.follower.done:
	default:
		t.Fatal("follow loop still running after Close")
	}
	if _, err := r.sub.Next(t.Context()); !errors.Is(err, changefeed.ErrClosed) {
		t.Errorf("Next after Close = %v, want ErrClosed", err)
	}
	write := backend.WriteRequest{Path: "/docs/late.md", ExpectedVersion: -1, Content: []byte("# Late\n")}
	if _, err := r.store.Publish(context.Background(), write); !errors.Is(err, backend.ErrClosed) {
		t.Errorf("Publish after Close = %v, want backend.ErrClosed", err)
	}
	r.store.Follow(r.store.servedSequence() + 1)
	if err := r.store.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// Without WATCH there is nothing to follow for: no loop to start or stop.
func TestStoreWithoutWatchDoesNotFollow(t *testing.T) {
	store := (&bucketSite{objects: initializedMemory(t)}).open(t, 0)
	if store.follower != nil {
		t.Fatal("a store without WATCH started a follow loop")
	}
	store.Follow(store.servedSequence() + 1)
}

// Opening an existing world reads its marker once; only an empty bucket pays
// for the genesis check.
func TestOpenReadsAnExistingHeadOnce(t *testing.T) {
	objects := newObservedBlobStore(initializedMemory(t))
	if _, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID}); err != nil {
		t.Fatal(err)
	}
	counts := objects.counts()
	if reads := counts.heads[markerKey] + counts.gets[markerKey]; reads != 1 {
		t.Errorf("marker reads on open = %d (%d head, %d get), want 1", reads, counts.heads[markerKey], counts.gets[markerKey])
	}
}

// Close waits for the slot being created and refuses a write staged behind
// it: nothing lands after Close returns.
func TestCloseFencesCommits(t *testing.T) {
	objects := initializedMemory(t)
	staged := &blobCreates{Store: objects, created: make(chan struct{}, 4)}
	holds := newSlotHolds(t, staged, 2)
	store := (&bucketSite{objects: holds, noHedge: true}).open(t, 0)
	publish := func(path string) <-chan writeOutcome {
		return publishAsync(context.Background(), store, backend.WriteRequest{Path: path, ExpectedVersion: -1, Content: []byte("# " + path + "\n")})
	}
	first := publish("/first.md")
	waitForTestSignal(t, holds.holds[2].arrived, "the first slot create")
	<-staged.created
	late := publish("/late.md")
	waitForTestSignal(t, staged.created, "the late write staged behind the slot")

	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	waitUntil(t, store, "Close", func(queue *commitQueue) bool { return queue.closed })
	// Close cannot return while the slot create is held; the window only
	// gives a Close that skips it time to show it.
	select {
	case <-closed:
		t.Fatal("Close returned while a slot create was in flight")
	case <-time.After(20 * time.Millisecond):
	}
	holds.holds[2].open()
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}
	if outcome := <-first; outcome.err != nil {
		t.Fatalf("write whose slot was in flight: %v", outcome.err)
	}
	if outcome := <-late; !errors.Is(outcome.err, backend.ErrClosed) {
		t.Fatalf("write staged behind Close = %v, want backend.ErrClosed", outcome.err)
	}
	assertNoSlot(t, objects, 3)
	if outcome := <-publish("/after.md"); !errors.Is(outcome.err, backend.ErrClosed) {
		t.Fatalf("write after Close = %v, want backend.ErrClosed", outcome.err)
	}
}
