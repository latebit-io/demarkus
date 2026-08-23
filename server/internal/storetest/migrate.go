package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// MigrationBackend pairs a backend's migration surface with its read surface
// so the imported world can be checked behaviorally, not just byte-wise.
type MigrationBackend struct {
	Migrator store.Migrator
	Store    handler.DocumentStore
}

// RunMigrationRoundTrip migrates a seeded file store into the backend under
// test and back into a fresh file root; every stored version must survive
// both hops byte-for-byte. The imported world stays for caller assertions.
func RunMigrationRoundTrip(t *testing.T, newBackend func(t *testing.T) MigrationBackend) {
	src := store.New(t.TempDir())
	paths := seedMigrationDocs(t, src)
	srcExport := exportDocs(t, src)

	ctx := context.Background()
	b := newBackend(t)
	copyDocs(t, "src->backend", src, b.Migrator)
	if err := store.DiffExports(srcExport, exportDocs(t, b.Migrator)); err != nil {
		t.Fatalf("backend export differs from source: %v", err)
	}

	// The imported world behaves: chains verify and reads match the source.
	for _, p := range paths {
		if err := b.Store.VerifyChain(p); err != nil {
			t.Errorf("VerifyChain(%s): %v", p, err)
		}
		want, err := src.Get(p, 0)
		if err != nil {
			t.Fatalf("src Get(%s): %v", p, err)
		}
		got, err := b.Store.Get(p, 0)
		if err != nil {
			t.Fatalf("backend Get(%s): %v", p, err)
		}
		if !bytes.Equal(got.Content, want.Content) || got.Version != want.Version || got.Archived != want.Archived {
			t.Errorf("Get(%s): got v%d archived=%v, want v%d archived=%v", p, got.Version, got.Archived, want.Version, want.Archived)
		}
	}

	// The pruned document's history starts past v1 and must survive as-is.
	vs, err := b.Store.Versions("/pruned.md")
	if err != nil || len(vs) != 2 || vs[0].Version != 4 || vs[1].Version != 3 {
		t.Errorf("pruned versions: %v (err %v), want [4 3]", vs, err)
	}

	// Back into a fresh file root: byte-identical to the original.
	dst := store.New(t.TempDir())
	copyDocs(t, "backend->file", b.Migrator, dst)
	if err := store.DiffExports(srcExport, exportDocs(t, dst)); err != nil {
		t.Fatalf("round-tripped export differs from source: %v", err)
	}
	for _, p := range paths {
		if err := dst.VerifyChain(p); err != nil {
			t.Errorf("dst VerifyChain(%s): %v", p, err)
		}
	}

	// Re-importing an existing document must refuse, not overwrite.
	if err := b.Migrator.ImportDoc(ctx, "/a.md", srcExport["/a.md"]); !errors.Is(err, os.ErrExist) {
		t.Errorf("double import: err = %v, want ErrExist", err)
	}
	// Invalid imports must refuse on every backend.
	for name, bad := range map[string]struct {
		path     string
		document store.StoredDocument
	}{
		// Distinct paths: a wrongly accepted case must not mask the next
		// one behind ErrExist (map order is random).
		"no versions": {"/inv-empty.md", store.StoredDocument{}},
		"not ascending": {"/inv-order.md", store.StoredDocument{Versions: []store.StoredVersion{
			srcExport["/a.md"].Versions[1], srcExport["/a.md"].Versions[0],
		}}},
		"directory path": {"/inv-dir/", srcExport["/a.md"]},
		"oversized": {"/inv-big.md", store.StoredDocument{Versions: []store.StoredVersion{{
			Version: 1, Stored: make([]byte, protocol.MaxBodyLength+store.MaxStoreFrontmatter+1),
		}}}},
	} {
		if err := b.Migrator.ImportDoc(ctx, bad.path, bad.document); err == nil {
			t.Errorf("import %s: expected error", name)
		}
	}
}

// seedMigrationDocs writes a world exercising every migration-relevant shape:
// multi-version, append, nested path, unicode, archived doc, pruned history.
func seedMigrationDocs(t *testing.T, s *store.Store) []string {
	t.Helper()
	mustWrite := func(path string, version int, body string, meta map[string]string) {
		t.Helper()
		if _, err := s.WriteVersion(path, version, []byte(body), meta); err != nil {
			t.Fatalf("seed write %s: %v", path, err)
		}
	}
	mustWrite("/a.md", 0, "# A v1\n", map[string]string{"tags": "go,auth", "importance": "0.7"})
	mustWrite("/a.md", 1, "# A v2\n", map[string]string{"tags": "go,auth", "importance": "0.7"})
	if _, err := s.Append("/a.md", 2, []byte("appended\n"), nil); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	mustWrite("/dir/deep.md", 0, "# Deep\n", nil)
	mustWrite("/uni.md", 0, "# Ünïcode ☃ 日本語\n", map[string]string{"tags": "unicode"})
	mustWrite("/arch.md", 0, "# Archived\n", nil)
	if _, _, err := s.ArchiveResult("/arch.md", true); err != nil {
		t.Fatalf("seed archive: %v", err)
	}
	for i := range 4 {
		mustWrite("/pruned.md", i, fmt.Sprintf("# P v%d\n", i+1), map[string]string{"retention": "2"})
	}
	return []string{"/a.md", "/dir/deep.md", "/uni.md", "/arch.md", "/pruned.md"}
}

// exportDocs collects a migrator's full export keyed by path.
func exportDocs(t *testing.T, m store.Migrator) map[string]store.StoredDocument {
	t.Helper()
	out := map[string]store.StoredDocument{}
	if err := m.ExportDocs(context.Background(), func(p string, document store.StoredDocument) error {
		out[p] = document
		return nil
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	return out
}

// copyDocs migrates every document from src to dst.
func copyDocs(t *testing.T, label string, src, dst store.Migrator) {
	t.Helper()
	ctx := context.Background()
	if err := src.ExportDocs(ctx, func(p string, document store.StoredDocument) error {
		return dst.ImportDoc(ctx, p, document)
	}); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}
