package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
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

	// A replica opened later replays the receipt window, so a watcher that
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
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if ev := b.next(t); ev.Path != "/docs/one.md" || ev.Seq != 2 {
		t.Fatalf("hint after a read = %+v", ev)
	}
}

// Commits beyond the receipt window cannot be named, so a replica that
// missed them tells its watchers to resync from the covered sequence.
func TestGapBeyondReceiptsResyncs(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects)
	b := openReplica(t, objects)
	for i := range maximumReceipts + 2 {
		if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: -1, Content: fmt.Appendf(nil, "# %d\n", i)}); err != nil {
			t.Fatal(err)
		}
	}
	b.poll(t)
	rctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := b.sub.Next(rctx); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("subscriber across the gap got %v, want ErrResync", err)
	}
	if b.hub.Head() != a.hub.Head() {
		t.Fatalf("heads differ after the gap: %v vs %v", b.hub.Head(), a.hub.Head())
	}
	fresh, err := b.hub.Subscribe(t.Context(), "/", b.hub.Head())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/two.md", ExpectedVersion: -1, Content: []byte("# Two\n")}); err != nil {
		t.Fatal(err)
	}
	b.poll(t)
	if ev, err := fresh.Next(rctx); err != nil || ev.Path != "/docs/two.md" || ev.Seq != a.hub.Head().Seq {
		t.Fatalf("event after the resync = %+v, %v", ev, err)
	}
}

// A receipt written before receipts named their change is a gap, not a
// guess.
func TestUnnamedReceiptSkips(t *testing.T) {
	hub := changefeed.New(testWorldID, 0)
	store := &Store{changes: hub}
	sub, _ := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	store.report(&snapshot{
		Head: headObject{Sequence: 3, Receipts: []operationReceipt{
			{Sequence: 2, Result: "committed"},
			{Sequence: 3, Result: "committed", Path: "/a.md", Op: protocol.OpPublish, Version: 1, Hash: "sha256-x"},
		}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("across the unnamed receipt got %v, want ErrResync", err)
	}
	resumed, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: testWorldID, Seq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := resumed.Next(ctx); err != nil || ev.Seq != 3 || ev.Path != "/a.md" || ev.Version != 1 {
		t.Fatalf("named receipt = %+v, %v", ev, err)
	}
}

// A receipt carries its own commit's version and hash: a replica that
// catches up on two commits to one path reports each as it was, not both as
// the path is now.
func TestReceiptsKeepTheirCommit(t *testing.T) {
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
// hub, as a peer replica does; the head's receipts name it on reopen.
type bucketSite struct{ objects blob.Store }

func (s *bucketSite) open(t *testing.T, ring int) *Store {
	t.Helper()
	store, err := Open(context.Background(), s.objects, Options{Logger: discardLogger, WorldID: testWorldID, ChangeRing: ring})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.commitInterval = 0
	return store
}

// siteRing holds the suite's 48 concurrent events, and a resume across
// reopen reaches past the receipts into sealed change blocks.
const siteRing = 4 * maximumReceipts

func (s *bucketSite) Open(t *testing.T) storetest.ChangeBackend {
	return storetest.ChangeBackend{Store: s.open(t, siteRing)}
}

func (s *bucketSite) Tamper(t *testing.T, path string) {
	t.Helper()
	if _, err := s.open(t, 0).Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# peer\n")}); err != nil {
		t.Fatalf("tamper %s: %v", path, err)
	}
}

// Window is the ring: receipts, then sealed blocks, name the rest.
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
