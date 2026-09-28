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
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
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
	hub := changefeed.New(testWorldID, 0)
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, Changes: hub})
	if err != nil {
		t.Fatalf("open replica: %v", err)
	}
	store.commitInterval = 0
	sub, err := hub.Subscribe("/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	return replica{store: store, hub: hub, sub: sub}
}

func (r replica) next(t *testing.T) changefeed.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ev, err := r.sub.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

func (r replica) quiet(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if ev, err := r.sub.Next(ctx); err == nil {
		t.Fatalf("unexpected event %+v", ev)
	}
}

func (r replica) poll(t *testing.T) {
	t.Helper()
	if err := r.store.Poll(context.Background()); err != nil {
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
	moved, err := c.hub.Subscribe("/", protocol.Cursor{Epoch: testWorldID, Seq: 3})
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
	fresh, err := b.hub.Subscribe("/", b.hub.Head())
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
	sub, _ := hub.Subscribe("/", protocol.Cursor{})
	store.report(&snapshot{
		Head: headObject{Sequence: 3, Receipts: []operationReceipt{
			{Sequence: 2, Result: "committed"},
			{Sequence: 3, Result: "committed", Path: "/a.md", Op: protocol.OpPublish},
		}},
		Paths: map[string]snapshotEntry{"/a.md": {Current: 1, BodyHash: "sha256-x"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sub.Next(ctx); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("across the unnamed receipt got %v, want ErrResync", err)
	}
	resumed, err := hub.Subscribe("/", protocol.Cursor{Epoch: testWorldID, Seq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := resumed.Next(ctx); err != nil || ev.Seq != 3 || ev.Path != "/a.md" || ev.Version != 1 {
		t.Fatalf("named receipt = %+v, %v", ev, err)
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
