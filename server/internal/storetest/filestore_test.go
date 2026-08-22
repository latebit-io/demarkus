package storetest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// TestFileStoreConformance runs the conformance suite against the file
// store, pinning the reference behavior every other backend must match.
func TestFileStoreConformance(t *testing.T) {
	RunConformance(t, func(t *testing.T) handler.DocumentStore {
		return store.New(t.TempDir())
	}, FileTamper)
}

// TestFileStoreLookupConformance runs the LOOKUP conformance suite against
// the file backend's pairing: file store plus in-memory catalog, kept in
// sync by the handler-mirroring helpers.
func TestFileStoreLookupConformance(t *testing.T) {
	RunLookupConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
}

// TestFileStoreDifferentialSelf runs the differential harness with the file
// store on both sides. It proves the harness itself is deterministic and
// backend-neutral: a self-diff failure is a harness bug, not a store bug.
func TestFileStoreDifferentialSelf(t *testing.T) {
	factory := func(t *testing.T) LookupBackend { return FileBackend(t) }
	RunDifferential(t, factory, factory, DifferentialConfig{Seeds: []int64{1, 2}, Ops: 80})
}

// TestFileStoreHandlerDifferentialSelf proves the handler-level harness is
// deterministic with the file store on both sides.
func TestFileStoreHandlerDifferentialSelf(t *testing.T) {
	factory := func(t *testing.T) LookupBackend { return FileBackend(t) }
	RunHandlerDifferential(t, factory, factory, DifferentialConfig{Seeds: []int64{1, 2}, Ops: 80})
}

// TestFileStoreMigrationRoundTrip runs the shared migration gate with the
// file store as the backend under test (file -> file -> file).
func TestFileStoreMigrationRoundTrip(t *testing.T) {
	RunMigrationRoundTrip(t, func(t *testing.T) MigrationBackend {
		s := store.New(t.TempDir())
		return MigrationBackend{Migrator: s, Store: s}
	})
}

func BenchmarkFileHandler(b *testing.B) {
	RunHandlerBenchmarks(b, FileBackend)
}

// TestFileStoreImportRefusesOrphanedVersionDir pins the failure-cleanup
// guard: an import onto a leftover version tree (no current symlink) must
// refuse with ErrExist and leave the pre-existing version files untouched.
func TestFileStoreImportRefusesOrphanedVersionDir(t *testing.T) {
	s := store.New(t.TempDir())
	if _, err := s.WriteVersion("/x.md", 0, []byte("# X\n"), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	vFile, err := s.VersionFilePath("/x.md", 1)
	if err != nil {
		t.Fatalf("version path: %v", err)
	}
	before, err := os.ReadFile(vFile)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	// Orphan the tree: remove the current symlink, keep the version files.
	if err := os.Remove(filepath.Join(s.Root(), "x.md")); err != nil {
		t.Fatalf("orphan: %v", err)
	}

	err = s.ImportDoc(context.Background(), "/x.md", store.StoredDocument{Versions: []store.StoredVersion{{
		Version: 1, Stored: []byte("# other\n"), Modified: time.Now(),
	}}})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("import onto orphaned tree: err = %v, want ErrExist", err)
	}
	after, err := os.ReadFile(vFile)
	if err != nil {
		t.Fatalf("orphaned version file gone after refused import: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("orphaned version file modified by refused import")
	}
}
