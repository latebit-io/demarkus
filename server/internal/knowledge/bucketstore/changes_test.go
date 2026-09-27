package bucketstore

import (
	"context"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

// replica is one store over the shared bucket with its own hub and a
// subscription over everything.
type replica struct {
	store *Store
	hub   *changefeed.Hub
	sub   *changefeed.Subscription
}

func openReplica(t *testing.T, objects blob.Store, epoch string) replica {
	t.Helper()
	hub := changefeed.New(epoch, 0)
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, Changes: hub})
	if err != nil {
		t.Fatalf("open %s: %v", epoch, err)
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

// A write through one replica is a hint on it at once and on the other
// after a poll; the poll reports what the snapshot shows and nothing twice.
func TestPollReportsPeerWrites(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects, "a")
	b := openReplica(t, objects, "b")
	body := []byte("# One\n")

	doc, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: -1, Content: body, Metadata: map[string]string{"agent": "session-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if ev := a.next(t); ev.Path != "/docs/one.md" || ev.Version != 1 || ev.Op != protocol.OpPublish || ev.Agent != "session-a" || ev.Hash != storefmt.ContentHash(body) {
		t.Fatalf("local hint = %+v", ev)
	}
	b.quiet(t)
	if err := b.store.Poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if ev := b.next(t); ev.Path != "/docs/one.md" || ev.Version != 1 || ev.Op != protocol.OpPublish || ev.Hash != storefmt.ContentHash(body) || ev.Agent != "" {
		t.Fatalf("peer hint = %+v", ev)
	}
	if err := b.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	b.quiet(t)

	// An append is an append where it happened and a publish where it is
	// only seen in the snapshot.
	appended, err := a.store.Append(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: doc.Version, Content: []byte("more\n")})
	if err != nil {
		t.Fatal(err)
	}
	if ev := a.next(t); ev.Op != protocol.OpAppend || ev.Version != 2 {
		t.Fatalf("local append hint = %+v", ev)
	}
	if err := b.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := b.next(t); ev.Op != protocol.OpPublish || ev.Version != 2 || ev.Hash != storefmt.ContentHash(appended.Content) {
		t.Fatalf("peer append hint = %+v", ev)
	}

	// Archive and unarchive are told apart by the flag.
	if _, err := a.store.SetArchived(ctx, backend.ArchiveRequest{Path: "/docs/one.md", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if ev := a.next(t); ev.Op != protocol.OpArchive || ev.Version != 2 {
		t.Fatalf("local archive hint = %+v", ev)
	}
	if err := b.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := b.next(t); ev.Op != protocol.OpArchive || ev.Version != 2 {
		t.Fatalf("peer archive hint = %+v", ev)
	}
	if _, err := a.store.SetArchived(ctx, backend.ArchiveRequest{Path: "/docs/one.md", Archived: false}); err != nil {
		t.Fatal(err)
	}
	if ev := a.next(t); ev.Op != protocol.OpPublish || ev.Version != 2 {
		t.Fatalf("local unarchive hint = %+v", ev)
	}
	if err := b.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := b.next(t); ev.Op != protocol.OpPublish || ev.Version != 2 {
		t.Fatalf("peer unarchive hint = %+v", ev)
	}

	// A's own writes are never re-reported by its polls, and a replica
	// opened later takes the world as its baseline.
	if err := a.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	a.quiet(t)
	c := openReplica(t, objects, "c")
	if err := c.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	c.quiet(t)
	if _, err := c.store.Publish(ctx, backend.WriteRequest{Path: "/docs/two.md", ExpectedVersion: -1, Content: body}); err != nil {
		t.Fatal(err)
	}
	if ev := c.next(t); ev.Path != "/docs/two.md" {
		t.Fatalf("hint on the new replica = %+v", ev)
	}
	if err := a.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := a.next(t); ev.Path != "/docs/two.md" || ev.Op != protocol.OpPublish {
		t.Fatalf("a's hint for c's write = %+v", ev)
	}
}

// A poll that took its snapshot before a local commit must not announce the
// path's older state after the commit's own hint.
func TestStaleSnapshotDoesNotRepeatALocalWrite(t *testing.T) {
	objects := initializedMemory(t)
	ctx := context.Background()
	a := openReplica(t, objects, "a")
	if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: -1, Content: []byte("# One\n")}); err != nil {
		t.Fatal(err)
	}
	a.next(t)
	before := a.store.snapshot.Load()
	if _, err := a.store.Publish(ctx, backend.WriteRequest{Path: "/docs/one.md", ExpectedVersion: 1, Content: []byte("# Two\n")}); err != nil {
		t.Fatal(err)
	}
	if ev := a.next(t); ev.Version != 2 {
		t.Fatalf("local hint = %+v", ev)
	}
	a.store.report(before)
	a.quiet(t)
	if err := a.store.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	a.quiet(t)
}
