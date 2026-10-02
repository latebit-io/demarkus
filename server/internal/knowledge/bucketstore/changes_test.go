package bucketstore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// replica is one store over the shared bucket with its own hub, under the
// world's epoch, and a subscription over everything.
type replica struct {
	store *Store
	hub   *changefeed.Hub
	sub   *changefeed.Subscription
}

func openReplica(t *testing.T, objects blob.Store) replica {
	t.Helper()
	store := (&bucketSite{objects: objects}).open(t, changefeed.DefaultRingSize)
	hub := store.Changes()
	sub, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	return replica{store: store, hub: hub, sub: sub}
}

func (r replica) next(t *testing.T) changefeed.Event { return storetest.NextEvent(t, r.sub) }

func (r replica) quiet(t *testing.T) { storetest.Quiet(t, r.sub) }

func (r replica) poll(t *testing.T) {
	t.Helper()
	if err := r.store.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// Every replica reports a commit under its head sequence with the exact op
// and agent, whether it made the commit, polled it, or opened afterwards;
// a cursor from one replica resumes on another.
func TestReplicasShareOneSequence(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects)
	b := openReplica(t, objects)
	body := []byte("# One\n")

	doc, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: -1, Content: body, Metadata: map[string]string{"agent": "session-a"}})
	if err != nil {
		t.Fatal(err)
	}
	want := changefeed.Event{Seq: 2, Path: "/docs/one.md", Version: 1, Hash: storefmt.ContentHash(body), Op: protocol.OpPublish, Agent: "session-a"}
	if ev := a.next(t); ev != want {
		t.Fatalf("local hint = %+v, want %+v", ev, want)
	}
	b.quiet(t)
	b.poll(t)
	if ev := b.next(t); ev != want {
		t.Fatalf("peer hint = %+v, want %+v", ev, want)
	}
	b.poll(t)
	b.quiet(t)
	if a.hub.Head() != b.hub.Head() {
		t.Fatalf("heads differ: %v vs %v", a.hub.Head(), b.hub.Head())
	}

	appended, err := a.store.Append(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: doc.Version, Content: []byte("more\n")})
	if err != nil {
		t.Fatal(err)
	}
	want = changefeed.Event{Seq: 3, Path: "/docs/one.md", Version: 2, Hash: storefmt.ContentHash(appended.Content), Op: protocol.OpAppend, Agent: "session-a"}
	if ev := a.next(t); ev != want {
		t.Fatalf("local append hint = %+v, want %+v", ev, want)
	}
	b.poll(t)
	if ev := b.next(t); ev != want {
		t.Fatalf("peer append hint = %+v, want %+v", ev, want)
	}
	if _, err := a.store.SetArchived(ctx, backend.ArchiveRequest{Path: "/docs/one.md", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.SetArchived(ctx, backend.ArchiveRequest{Path: "/docs/one.md", Archived: false}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{protocol.OpArchive, protocol.OpPublish} {
		if ev := a.next(t); ev.Op != op || ev.Version != 2 {
			t.Fatalf("local %s hint = %+v", op, ev)
		}
	}
	b.poll(t)
	for _, op := range []string{protocol.OpArchive, protocol.OpPublish} {
		if ev := b.next(t); ev.Op != op || ev.Version != 2 {
			t.Fatalf("peer %s hint = %+v", op, ev)
		}
	}
	a.poll(t)
	a.quiet(t)

	// A replica opened later replays the slots, so a watcher that
	// moves to it resumes from its cursor with the events it has not seen.
	c := openReplica(t, objects)
	c.quiet(t)
	if c.hub.Head() != a.hub.Head() {
		t.Fatalf("late replica head = %v, want %v", c.hub.Head(), a.hub.Head())
	}
	moved, err := c.hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: testWorldID, Seq: 3})
	if err != nil {
		t.Fatalf("resume on the late replica: %v", err)
	}
	for _, op := range []string{protocol.OpArchive, protocol.OpPublish} {
		ev, err := moved.Next(ctx)
		if err != nil || ev.Op != op {
			t.Fatalf("resumed event = %+v, %v; want op %s", ev, err, op)
		}
	}
}

// A read on a replica refreshes its snapshot, and that alone reports what
// peers committed: no poll needed while the world is being read.
func TestReadRefreshReportsPeerWrites(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects)
	b := openReplica(t, objects)
	if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: -1, Content: []byte("# One\n")}); err != nil {
		t.Fatal(err)
	}
	a.next(t)
	view, err := b.store.OpenReadView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Get(ctx, "/docs/one.md", 0); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if ev := b.next(t); ev.Path != "/docs/one.md" || ev.Seq != 2 {
		t.Fatalf("hint after a read = %+v", ev)
	}
}

// Slots carry every change, so a replica that missed many commits reports
// each of them in order when it catches up, with no gap to resync over.
func TestLaggingReplicaReportsEveryCommit(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects)
	b := openReplica(t, objects)
	const commits = 40
	for i := range commits {
		if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: -1, Content: fmt.Appendf(nil, "# %d\n", i)}); err != nil {
			t.Fatal(err)
		}
	}
	b.poll(t)
	for i := range commits {
		if ev := b.next(t); ev.Version != i+1 || ev.Path != "/docs/one.md" {
			t.Fatalf("event %d = %+v, want version %d", i, ev, i+1)
		}
	}
	if b.hub.Head() != a.hub.Head() {
		t.Fatalf("heads differ after catching up: %v vs %v", b.hub.Head(), a.hub.Head())
	}
}

// A slot entry carries its own commit's version and hash: a replica that
// catches up on two commits to one path reports each as it was, not both as
// the path is now.
func TestSlotEventsKeepTheirCommit(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects)
	b := openReplica(t, objects)
	first := []byte("# One\n")
	doc, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: -1, Content: first})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.store.Append(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: doc.Version, Content: []byte("more\n")})
	if err != nil {
		t.Fatal(err)
	}
	b.poll(t)
	if ev := b.next(t); ev.Version != 1 || ev.Hash != storefmt.ContentHash(first) {
		t.Fatalf("first hint = %+v, want version 1 with its own hash", ev)
	}
	if ev := b.next(t); ev.Version != 2 || ev.Hash != storefmt.ContentHash(second.Content) {
		t.Fatalf("second hint = %+v, want version 2 with its own hash", ev)
	}
}

// Hints leave in commit order even when writers race: each is reported
// where its snapshot is installed, under the refresh lock.
func TestLocalHintsFollowCommitOrder(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects)
	const writers, each = 8, 5
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range each {
				body := fmt.Appendf(nil, "# writer %d, write %d\n", w, i)
				if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/race.md", ExpectedVersion: -1, Content: body}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	for want := 1; want <= writers*each; want++ {
		if ev := a.next(t); ev.Version != want || ev.Op != protocol.OpPublish || ev.Seq != uint64(want+1) {
			t.Fatalf("hint %d = %+v", want, ev)
		}
	}
	a.quiet(t)
}

// bucketSite is one bucket. Tamper commits through a second store with no
// hub, as a peer replica does; the slots name it on reopen.
type bucketSite struct {
	objects blob.Store
	// follow overrides the backstop period of the stores it opens.
	follow time.Duration
}

// open opens a store that closes when t ends, stopping its follow loop.
func (s *bucketSite) open(t *testing.T, ring int) *Store {
	t.Helper()
	store, err := Open(context.Background(), s.objects, Options{Logger: discardLogger, WorldID: testWorldID, ChangeRing: ring, followInterval: s.follow})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closeAtEnd(t, store)
	return store
}

func closeAtEnd(t *testing.T, store *Store) {
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
}

// siteRing holds the suite's 48 concurrent events; a resume across reopen
// within it reads the slots.
const siteRing = 64

func (s *bucketSite) Open(t *testing.T) storetest.ChangeBackend {
	store := s.open(t, siteRing)
	return storetest.ChangeBackend{Store: store, Close: store.Close}
}

func (s *bucketSite) Tamper(t *testing.T, path string) {
	t.Helper()
	if _, err := s.open(t, 0).Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# peer\n")}); err != nil {
		t.Fatalf("tamper %s: %v", path, err)
	}
}

// Window is the ring: the slots name every change within it.
func (s *bucketSite) Window() int { return siteRing }

func TestChangeConformance(t *testing.T) {
	storetest.RunChangeConformance(t, func(t *testing.T) storetest.ChangeSite {
		return &bucketSite{objects: initializedMemory(t)}
	})
}

// A watcher that moves to a replica which has not polled yet resumes from
// its cursor: the hub catches up from the bucket instead of resyncing.
func TestResumeOnALaggingReplicaCatchesUp(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects)
	b := openReplica(t, objects)
	for _, path := range []string{"/one.md", "/two.md"} {
		if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: path, ExpectedVersion: -1, Content: []byte("# doc\n")}); err != nil {
			t.Fatal(err)
		}
	}
	seen := a.next(t)
	a.next(t)
	if b.hub.Head().Seq >= seen.Seq {
		t.Fatalf("replica b already at %v; the test needs it behind %d", b.hub.Head(), seen.Seq)
	}
	moved, err := b.hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: testWorldID, Seq: seen.Seq})
	if err != nil {
		t.Fatalf("resume on the lagging replica: %v", err)
	}
	if ev := storetest.NextEvent(t, moved); ev.Path != "/two.md" {
		t.Fatalf("resumed event = %+v, want /two.md", ev)
	}
}

// replicaSite opens stores over one bucket, as replicas of one world.
type replicaSite struct{ site *bucketSite }

func (s replicaSite) Open(t *testing.T) handler.DocumentStore { return s.site.open(t, 0) }

func TestReplicaConformance(t *testing.T) {
	storetest.RunReplicaConformance(t, func(t *testing.T) storetest.ReplicaSite {
		return replicaSite{site: &bucketSite{objects: initializedMemory(t)}}
	})
}
