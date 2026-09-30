package filestore

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

var testLogger = slog.New(slog.DiscardHandler)

// openWatched opens root with the hash index built and WATCH on.
func openWatched(t *testing.T, root string, ring int) (*Store, *changefeed.Hub) {
	t.Helper()
	documents, err := protocolstore.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := documents.BuildHashIndex(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(documents, catalog.New(), Options{ChangeRing: ring, Logger: testLogger})
	if err != nil {
		t.Fatal(err)
	}
	hub := store.Changes()
	return store, hub
}

func publishN(t *testing.T, store *Store, n int) {
	t.Helper()
	for i := range n {
		path := "/d/" + string(rune('a'+i)) + ".md"
		if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# " + path + "\n")}); err != nil {
			t.Fatal(err)
		}
	}
}

// readJournal parses the journal on disk.
func readJournal(t *testing.T, root string) (j *journal, clean bool) {
	t.Helper()
	j = &journal{path: filepath.Join(root, journalName), logger: testLogger}
	_, clean, err := j.read()
	if err != nil {
		t.Fatal(err)
	}
	return j, clean
}

func closeStore(t *testing.T, store *Store) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// The journal keeps one ring's worth: after enough appends it is rewritten,
// and a reopen resumes exactly the retained tail.
func TestJournalCompactsToTheRing(t *testing.T) {
	root := t.TempDir()
	store, hub := openWatched(t, root, 4)
	publishN(t, store, 10)
	epoch := hub.Epoch()
	closeStore(t, store)

	j, _ := readJournal(t, root)
	if tail := j.tail; len(tail) != 4 || tail[0].Seq != 7 || tail[3].Seq != 10 {
		t.Fatalf("journal holds %d entries from seq %d; want 4 from 7", len(tail), tail[0].Seq)
	}

	_, reopened := openWatched(t, root, 4)
	if reopened.Epoch() != epoch {
		t.Fatalf("epoch changed on reopen: %q -> %q", epoch, reopened.Epoch())
	}
	sub, err := reopened.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: epoch, Seq: 6})
	if err != nil {
		t.Fatalf("resume at the oldest retained: %v", err)
	}
	if ev, err := sub.Next(context.Background()); err != nil || ev.Seq != 7 {
		t.Fatalf("first replayed = %+v, %v; want seq 7", ev, err)
	}
	if _, err := reopened.Subscribe(t.Context(), "/", protocol.Cursor{Epoch: epoch, Seq: 5}); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("resume before the tail: err = %v, want ErrResync", err)
	}
}

// A torn tail (a crash mid-append) never breaks open: the readable prefix is
// parsed, the file is rewritten clean, and since the tree holds a commit the
// journal cannot name, the epoch changes.
func TestJournalTornTailStartsNewEpoch(t *testing.T) {
	root := t.TempDir()
	store, hub := openWatched(t, root, 8)
	publishN(t, store, 3)
	epoch := hub.Epoch()
	closeStore(t, store)

	path := filepath.Join(root, journalName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-20], 0o644); err != nil {
		t.Fatal(err)
	}
	if torn, clean := readJournal(t, root); clean || torn.epoch != epoch || len(torn.tail) != 2 {
		t.Fatalf("torn journal read: epoch %q, %d entries, clean %v; want %q, 2, false", torn.epoch, len(torn.tail), clean, epoch)
	}

	_, reopened := openWatched(t, root, 8)
	if reopened.Epoch() == epoch {
		t.Fatal("epoch kept although the last commit is missing from the journal")
	}
	if _, clean := readJournal(t, root); !clean {
		t.Fatal("journal not rewritten clean")
	}
}

// An archive made behind the journal changes the tree without adding a
// version; the fingerprint catches it too.
func TestOutOfBandArchiveStartsNewEpoch(t *testing.T) {
	root := t.TempDir()
	store, hub := openWatched(t, root, 8)
	publishN(t, store, 1)
	epoch, head := hub.Epoch(), hub.Head()
	closeStore(t, store)

	documents, err := protocolstore.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(documents, catalog.New()).SetArchived(context.Background(), backend.ArchiveRequest{Path: "/d/a.md", Archived: true}); err != nil {
		t.Fatal(err)
	}

	_, reopened := openWatched(t, root, 8)
	if reopened.Epoch() == epoch {
		t.Fatal("epoch kept although a document was archived behind the journal")
	}
	if _, err := reopened.Subscribe(t.Context(), "/", head); !errors.Is(err, changefeed.ErrResync) {
		t.Fatalf("resume after an out-of-band archive: err = %v, want ErrResync", err)
	}
}

// Close ends the watch and refuses later writes.
func TestCloseEndsWatchAndRefusesWrites(t *testing.T) {
	store, hub := openWatched(t, t.TempDir(), 8)
	sub, err := hub.Subscribe(t.Context(), "/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, store)
	if _, err := sub.Next(context.Background()); !errors.Is(err, changefeed.ErrClosed) {
		t.Fatalf("Next after Close: err = %v, want ErrClosed", err)
	}
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: "/late.md", Content: []byte("# late\n")}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close: err = %v, want ErrClosed", err)
	}
}

// A journal whose file was lost recovers on the next append by rewriting the
// whole tail, pending event included: the epoch and every event survive a
// reopen.
func TestJournalRecoversAfterLostFile(t *testing.T) {
	root := t.TempDir()
	store, hub := openWatched(t, root, 8)
	publishN(t, store, 1)
	epoch := hub.Epoch()
	store.journal.lose("test", errors.New("disk gone"))
	store.journal.retryAt = time.Time{}
	for _, path := range []string{"/r/two.md", "/r/three.md"} {
		if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# r\n")}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore(t, store)

	_, reopened := openWatched(t, root, 8)
	if reopened.Epoch() != epoch {
		t.Fatalf("epoch changed after recovery: %q -> %q", epoch, reopened.Epoch())
	}
	if j, _ := readJournal(t, root); len(j.tail) != 3 || j.tail[2].Seq != 3 {
		t.Fatalf("journal holds %d entries; want the 3 commits", len(j.tail))
	}
}
