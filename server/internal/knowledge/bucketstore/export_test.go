package bucketstore

import (
	"context"
	"errors"
	"slices"
	"testing"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
)

func TestExportDocsPreservesPinnedStoredDocuments(t *testing.T) {
	memory := initializedMemory(t)
	live := newReadDocument("/docs/live.md", "# Live v1\n", "# Live v2\n")
	archived := newReadDocument("/archive/old.md", "# Old\n")
	archived.Archived = true
	pruned := newReadDocument("/docs/pruned.md", "one", "two", "three")
	pruned.RetainFrom = 3
	specs := []readDocumentSpec{
		live,
		archived,
		pruned,
		newReadDocument("/docs/topic.md", "# Topic\n"),
		newReadDocument("/docs/topic/detail.md", "# Detail\n"),
	}
	commit := commitReadDocuments(t, memory, specs)
	want := make(map[string]protocolstore.StoredDocument, len(specs))
	for _, spec := range specs {
		versions := make([]protocolstore.StoredVersion, 0, len(spec.Bodies)-spec.RetainFrom+1)
		for version := spec.RetainFrom; version <= len(spec.Bodies); version++ {
			stored := commit.documents[spec.Path].versions[version]
			versions = append(versions, protocolstore.StoredVersion{Version: version, Stored: slices.Clone(stored.raw), Modified: stored.modified})
		}
		want[spec.Path] = protocolstore.StoredDocument{Versions: versions, Archived: spec.Archived}
	}
	got := make(map[string]protocolstore.StoredDocument)
	var paths []string
	if err := ExportDocs(t.Context(), memory, testWorldID, 2, func(path string, document protocolstore.StoredDocument) error {
		paths = append(paths, path)
		got[path] = document
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"/archive/old.md", "/docs/live.md", "/docs/pruned.md", "/docs/topic/detail.md", "/docs/topic.md"}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("export paths = %v, want store traversal order %v", paths, wantPaths)
	}
	if err := protocolstore.DiffExports(want, got); err != nil {
		t.Fatal(err)
	}
}

func TestExportDocsPinsRootBeforeCallbacks(t *testing.T) {
	writer, memory := newWritableStore(t)
	if _, err := writer.WriteVersion("/first.md", 0, []byte("# First\n"), nil); err != nil {
		t.Fatal(err)
	}
	var paths []string
	if err := ExportDocs(t.Context(), memory, testWorldID, 1, func(path string, _ protocolstore.StoredDocument) error {
		paths = append(paths, path)
		if _, err := writer.WriteVersion("/later.md", 0, []byte("# Later\n"), nil); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"/first.md"}) {
		t.Fatalf("export crossed roots: %v", paths)
	}
}

func TestExportDocsSurfacesCancellationAndCallbackFailure(t *testing.T) {
	memory := initializedMemory(t)
	commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/doc.md", "# Doc\n")})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ExportDocs(ctx, memory, testWorldID, 1, func(string, protocolstore.StoredDocument) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export error=%v", err)
	}
	want := errors.New("stop export")
	if err := ExportDocs(t.Context(), memory, testWorldID, 1, func(string, protocolstore.StoredDocument) error { return want }); !errors.Is(err, want) {
		t.Fatalf("callback error=%v", err)
	}
}
