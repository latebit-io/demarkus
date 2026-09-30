package filestore

import (
	"context"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// Every committed write, and only a committed one, becomes a hint with the
// stored version's hash, in commit order.
func TestCommitsPublishHints(t *testing.T) {
	store, hub := openWatched(t, t.TempDir(), changefeed.DefaultRingSize)
	sub, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	doc, err := store.Publish(ctx, backend.WriteRequest{Path: "/a.md", ExpectedVersion: -1, Content: []byte("# A\n"), Metadata: map[string]string{"agent": "claude-code"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Publish(ctx, backend.WriteRequest{Path: "/a.md", ExpectedVersion: 7, Content: []byte("# B\n")}); err == nil {
		t.Fatal("conflicting publish succeeded")
	}
	appended, err := store.Append(ctx, backend.WriteRequest{Path: "/a.md", ExpectedVersion: doc.Version, Content: []byte("more\n")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetArchived(ctx, backend.ArchiveRequest{Path: "/a.md", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if res, err := store.SetArchived(ctx, backend.ArchiveRequest{Path: "/a.md", Archived: true}); err != nil || res.Changed {
		t.Fatalf("repeated archive: changed=%v err=%v", res.Changed, err)
	}
	if _, err := store.SetArchived(ctx, backend.ArchiveRequest{Path: "/a.md", Archived: false}); err != nil {
		t.Fatal(err)
	}

	hash := storefmt.ContentHash(appended.Content)
	want := []changefeed.Event{
		{Seq: 1, Path: "/a.md", Version: 1, Hash: storefmt.ContentHash([]byte("# A\n")), Op: protocol.OpPublish, Agent: "claude-code"},
		{Seq: 2, Path: "/a.md", Version: 2, Hash: hash, Op: protocol.OpAppend, Agent: "claude-code"},
		{Seq: 3, Path: "/a.md", Version: 2, Hash: hash, Op: protocol.OpArchive, Agent: "claude-code"},
		{Seq: 4, Path: "/a.md", Version: 2, Hash: hash, Op: protocol.OpPublish, Agent: "claude-code"},
	}
	for _, w := range want {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		got, err := sub.Next(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got != w {
			t.Fatalf("event = %+v, want %+v", got, w)
		}
	}
	if hub.Head().Seq != 4 {
		t.Fatalf("head = %v, want seq 4: the refused write and the no-op archive emit nothing", hub.Head())
	}
}
