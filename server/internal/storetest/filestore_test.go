package storetest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

// TestFileStoreConformance runs the suite against the filestore backend that
// production serves; tampering reaches the raw store under it.
func TestFileStoreConformance(t *testing.T) {
	var mu sync.Mutex
	documents := map[handler.DocumentStore]*store.Store{}
	RunConformance(t, func(t *testing.T) handler.DocumentStore {
		raw := store.New(t.TempDir())
		wrapped := filestore.New(raw, catalog.New())
		mu.Lock()
		documents[wrapped] = raw
		mu.Unlock()
		return wrapped
	}, func(t testing.TB, s handler.DocumentStore, path string, version int, stored []byte) {
		mu.Lock()
		raw := documents[s]
		mu.Unlock()
		FileTamper(t, raw, path, version, stored)
	})
}

// TestFileStoreLookupConformance runs the LOOKUP conformance suite against
// the file backend's pairing: file store plus in-memory catalog, kept in
// sync by the handler-mirroring helpers.
func TestFileStoreLookupConformance(t *testing.T) {
	RunLookupConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
}

// TestFileStoreLookupHandlerConformance pins the LOOKUP wire contract for
// the match key on the file backend.
func TestFileStoreLookupHandlerConformance(t *testing.T) {
	RunLookupHandlerConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
}

// TestFileStoreHandlerConformance pins backend dependent wire behavior.
func TestFileStoreHandlerConformance(t *testing.T) {
	RunHandlerConformance(t, func(t *testing.T) LookupBackend { return FileBackend(t) })
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
		return MigrationBackend{Migrator: s, Store: filestore.New(s, catalog.New())}
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

	err = s.ImportDoc(context.Background(), "/x.md", storefmt.StoredDocument{Versions: []storefmt.StoredVersion{{
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

// TestFileStoreRejectionConformance: with the policy decorator above it the
// file backend refuses exactly as the bucket backend does. It has no quota.
func TestFileStoreRejectionConformance(t *testing.T) {
	RunRejectionConformance(t, RejectionFactories{
		Policy: func(t *testing.T) LookupBackend {
			b := FileBackend(t)
			policy := []byte("# Write Policy\n\nCurated.\n\nstrictness: block\nrequire_tags: domain\n")
			meta := map[string]string{"tags": "category:governance", "type": "Policy"}
			if _, err := b.direct().WriteVersion(publishpolicy.DocumentPath, 0, policy, meta); err != nil {
				t.Fatalf("seed policy: %v", err)
			}
			b.Store = writepolicy.Enforce(b.Store, writepolicy.Options{Require: true})
			return b
		},
	})
}
