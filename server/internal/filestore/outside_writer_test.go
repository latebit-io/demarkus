package filestore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	protocolstore "github.com/latebit-io/demarkus/server/store"
)

func lookupPaths(t *testing.T, store *filestore.Store, query string) []string {
	t.Helper()
	view, err := store.OpenReadView(context.Background())
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Errorf("close view: %v", err)
		}
	}()
	rows, err := view.Lookup(context.Background(), query, catalog.Options{})
	if err != nil {
		t.Fatalf("lookup %q: %v", query, err)
	}
	paths := make([]string, len(rows))
	for i := range rows {
		paths[i] = rows[i].Path
	}
	return paths
}

// outsideWrite writes path, body pairs under dir as a second process does,
// then moves the marker.
func outsideWrite(t *testing.T, dir string, docs ...string) {
	t.Helper()
	tool := protocolstore.New(dir)
	for i := 0; i < len(docs); i += 2 {
		if _, err := tool.Write(docs[i], []byte(docs[i+1]), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := tool.MarkChanged(); err != nil {
		t.Fatal(err)
	}
}

// A direct writer (demarkus-publish) that marks the root changed is served by
// a running read-only store without a restart: fetch, catalog and quota count.
func TestOutsideWriterIsServedAfterMarkChanged(t *testing.T) {
	dir := t.TempDir()
	tool := protocolstore.New(dir)
	if _, err := tool.Write("/old.md", []byte("# Walrus\n"), nil); err != nil {
		t.Fatal(err)
	}
	store := openDir(t, dir, filestore.Options{ReadOnly: true})
	if got := lookupPaths(t, store, "walrus"); len(got) != 1 {
		t.Fatalf("lookup before the outside write = %v", got)
	}

	outsideWrite(t, dir, "/new.md", "# Narwhal\n", "/old.md", "# Manatee\n")

	if got := lookupPaths(t, store, "narwhal"); len(got) != 1 || got[0] != "/new.md" {
		t.Errorf("lookup of the new document = %v, want /new.md", got)
	}
	if got := lookupPaths(t, store, "manatee"); len(got) != 1 || got[0] != "/old.md" {
		t.Errorf("lookup of the updated title = %v, want /old.md", got)
	}
	if got := lookupPaths(t, store, "walrus"); len(got) != 0 {
		t.Errorf("lookup of the replaced title = %v, want none", got)
	}
}

// On a writable store the outside change has no events: a watcher resumes
// into a resync instead of silently missing it.
func TestOutsideWriterResyncsWatchers(t *testing.T) {
	dir := t.TempDir()
	store := openDir(t, dir, filestore.Options{ChangeRing: changefeed.DefaultRingSize})
	if err := create(store, "/a.md"); err != nil {
		t.Fatal(err)
	}
	before := store.Changes().Head()
	outsideWrite(t, dir, "/b.md", "# B\n")
	view, err := store.OpenReadView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Get(context.Background(), "/b.md", 0); err != nil {
		t.Errorf("fetch of the outside write: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Changes().Subscribe(context.Background(), "/", before)
	if !errors.Is(err, changefeed.ErrResync) {
		t.Errorf("resume from before the outside write = %v, want ErrResync", err)
	}
	// The store's own next commit still numbers past the skipped sequence.
	if err := create(store, "/c.md"); err != nil {
		t.Fatal(err)
	}
	if head := store.Changes().Head(); head.Seq != before.Seq+2 {
		t.Errorf("head after the next commit = %d, want %d", head.Seq, before.Seq+2)
	}
}

// A watcher waiting on the hub learns of an outside write with no read to
// notice the marker for it: the poll rebuilds and the watch gets resync.
func TestOutsideWriterResyncsAWaitingWatcherWithoutARead(t *testing.T) {
	dir := t.TempDir()
	store := openDir(t, dir, filestore.PollOutsideEvery(filestore.Options{ChangeRing: changefeed.DefaultRingSize}, 10*time.Millisecond))
	if err := create(store, "/a.md"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := store.Changes().Subscribe(ctx, "/", store.Changes().Head())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	waited := make(chan error, 1)
	go func() {
		_, err := sub.Next(ctx)
		waited <- err
	}()

	outsideWrite(t, dir, "/b.md", "# B\n")
	if err := <-waited; !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("the waiting watch got %v, want ErrResync", err)
	}
	if got := lookupPaths(t, store, "B"); len(got) != 1 {
		t.Errorf("lookup after the poll rebuilt = %v, want /b.md", got)
	}
}
