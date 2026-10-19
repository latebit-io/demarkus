package filestore_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/filestore"
)

// capturedSoul is the upgrade capture's soul directory, as the plugin wrote it.
var capturedSoul = filepath.Join("..", "..", "..", "testdata", "upgrade", "home", ".demarkus", "soul")

func openDir(t *testing.T, dir string, opts filestore.Options) *filestore.Store {
	t.Helper()
	store, err := filestore.OpenDir(dir, opts)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return store
}

func create(store *filestore.Store, path string) error {
	_, err := store.Publish(context.Background(), backend.WriteRequest{Path: path, Content: []byte("# Doc\n"), Metadata: map[string]string{}})
	return err
}

// TestOpenDirServesAnExistingSoul opens a copy of the captured soul through
// the one constructor both servers use and reads what the soul wrote.
func TestOpenDirServesAnExistingSoul(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(capturedSoul)); err != nil {
		t.Fatalf("copy the captured soul: %v", err)
	}
	store := openDir(t, dir, filestore.Options{ChangeRing: changefeed.DefaultRingSize, Logger: slog.Default()})
	doc, err := backendtest.Direct{Store: store}.Get("/index.md", 0)
	if err != nil {
		t.Fatalf("fetch the soul's index: %v", err)
	}
	if doc.Version < 1 || len(doc.Content) == 0 {
		t.Errorf("index.md: version %d, %d bytes; want the captured document", doc.Version, len(doc.Content))
	}
	if store.Changes() == nil {
		t.Error("OpenDir with a change ring: no change hub")
	}
}

// A writable directory is created; a read-only one must exist and is never
// written, not even the change journal.
func TestOpenDirCreatesOrRequiresTheDirectory(t *testing.T) {
	created := filepath.Join(t.TempDir(), "new", "world")
	openDir(t, created, filestore.Options{})
	if _, err := os.Stat(created); err != nil {
		t.Errorf("writable open did not create the directory: %v", err)
	}

	absent := filepath.Join(t.TempDir(), "absent")
	if _, err := filestore.OpenDir(absent, filestore.Options{ReadOnly: true}); err == nil {
		t.Fatal("read-only OpenDir on a missing directory: want an error")
	}
	empty := t.TempDir()
	store := openDir(t, empty, filestore.Options{ReadOnly: true, ChangeRing: changefeed.DefaultRingSize})
	if err := create(store, "/a.md"); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("publish on a read-only store = %v, want ErrReadOnly", err)
	}
	if _, err := store.SetArchived(context.Background(), backend.ArchiveRequest{Path: "/a.md", Archived: true}); !errors.Is(err, backend.ErrReadOnly) {
		t.Errorf("archive on a read-only store = %v, want ErrReadOnly", err)
	}
	entries, err := os.ReadDir(empty)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 || store.Changes() != nil {
		t.Errorf("read-only open wrote %d entries and a change hub %v; want neither", len(entries), store.Changes() != nil)
	}
}

// An entry the open walk could not index still counts against the cap: the
// document exists on disk, the index just cannot see it.
func TestOpenDirCountsSkippedEntriesAgainstTheCap(t *testing.T) {
	dir := t.TempDir()
	store := openDir(t, dir, filestore.Options{MaxDocuments: 2})
	if err := create(store, "/good.md"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Symlink("versions/missing.md/v1", filepath.Join(dir, "dangling.md")); err != nil {
		t.Fatal(err)
	}
	reopened := openDir(t, dir, filestore.Options{MaxDocuments: 2})
	if err := create(reopened, "/third.md"); !errors.Is(err, backend.ErrQuota) {
		t.Errorf("create with one indexed and one skipped entry under a cap of 2 = %v, want ErrQuota", err)
	}
}

// MaxDocuments caps creates; a new version of an existing path always lands.
func TestOpenDirCapsDocuments(t *testing.T) {
	store := openDir(t, t.TempDir(), filestore.Options{MaxDocuments: 1})
	if err := create(store, "/a.md"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := create(store, "/b.md"); !errors.Is(err, backend.ErrQuota) {
		t.Errorf("second create = %v, want ErrQuota", err)
	}
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: "/a.md", ExpectedVersion: 1, Content: []byte("# Doc 2\n"), Metadata: map[string]string{}}); err != nil {
		t.Errorf("new version under the cap: %v", err)
	}
	// An absent expected-version is an unconditional write (-1); it creates
	// a path and so meets the cap like any create.
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: "/c.md", ExpectedVersion: -1, Content: []byte("# Doc\n"), Metadata: map[string]string{}}); !errors.Is(err, backend.ErrQuota) {
		t.Errorf("unconditional create past the cap = %v, want ErrQuota", err)
	}
	if _, err := store.Publish(context.Background(), backend.WriteRequest{Path: "/a.md", ExpectedVersion: -1, Content: []byte("# Doc 3\n"), Metadata: map[string]string{}}); err != nil {
		t.Errorf("unconditional new version under the cap: %v", err)
	}
}
