package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// ReplicaSite is one world that several replicas of a store serve.
type ReplicaSite interface {
	// Open opens another replica over the site's world.
	Open(t *testing.T) handler.DocumentStore
}

// RunReplicaConformance proves backend.SearchFreshness for a store several
// replicas share: reads by path see every acknowledged write, catalog reads
// trail another replica by at most the bound, and a view reads one snapshot.
func RunReplicaConformance(t *testing.T, newSite func(t *testing.T) ReplicaSite) {
	subtests := []struct {
		name string
		run  func(*testing.T, ReplicaSite)
	}{
		{"PathReadsAreExact", testReplicaPathReadsAreExact},
		{"CatalogReadsWithinBound", testReplicaCatalogReadsWithinBound},
		{"OwnWritesAtOnce", testReplicaOwnWritesAtOnce},
		{"OneSnapshotPerView", testReplicaOneSnapshotPerView},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) { st.run(t, newSite(t)) })
	}
}

// replicaPair opens a writer and a reader whose snapshot is warm: it has
// served a catalog read, so a lagging store would answer from it.
func replicaPair(t *testing.T, site ReplicaSite) (writer, reader Direct) {
	t.Helper()
	writer, reader = Direct{Store: site.Open(t)}, Direct{Store: site.Open(t)}
	if _, err := reader.Lookup("warm", catalog.Options{}); err != nil {
		t.Fatalf("warm the reader: %v", err)
	}
	return writer, reader
}

func testReplicaPathReadsAreExact(t *testing.T, site ReplicaSite) {
	writer, reader := replicaPair(t, site)
	mustWrite(t, writer, "/exact/doc.md", 0, "# doc\n")
	if isDir, err := reader.IsDir("/exact"); err != nil || !isDir {
		t.Fatalf("reader IsDir right after the write = %v, %v; want a directory", isDir, err)
	}
	mustWrite(t, writer, "/exact.md", 0, "# v1\n")
	if got := currentVersion(t, reader, "/exact.md"); got != 1 {
		t.Fatalf("reader version = %d right after the write, want 1", got)
	}
	mustWrite(t, writer, "/exact.md", 1, "# v2\n")
	doc, err := reader.Get("/exact.md", 0)
	if err != nil || doc.Version != 2 || string(doc.Content) != "# v2\n" {
		t.Fatalf("reader Get = %+v, %v; want v2 at once", doc, err)
	}
	if err := reader.VerifyChain("/exact.md"); err != nil {
		t.Errorf("reader VerifyChain: %v", err)
	}
	// A write on the reader judges its expected version against the writer's.
	if _, err := reader.WriteVersion("/exact.md", 1, []byte("# stale\n"), nil); !errors.Is(err, storefmt.ErrConflict) {
		t.Errorf("stale write on the reader = %v, want conflict", err)
	}
}

func testReplicaCatalogReadsWithinBound(t *testing.T, site ReplicaSite) {
	writer, reader := replicaPair(t, site)
	if _, err := writer.WriteVersion("/bound/doc.md", 0, []byte("# Bound\n"), map[string]string{"tags": "freshness"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	acknowledged := time.Now()
	time.Sleep(time.Until(acknowledged.Add(backend.SearchFreshness)))

	results, err := reader.Lookup("freshness", catalog.Options{})
	if err != nil || len(results) != 1 || results[0].Path != "/bound/doc.md" {
		t.Errorf("Lookup %v after the write = %+v, %v; want the document", backend.SearchFreshness, results, err)
	}
	entries, err := reader.ListEntries("/bound/", false)
	if err != nil || !slices.ContainsFunc(entries, func(entry storefmt.DirEntry) bool { return entry.Name == "doc.md" }) {
		t.Errorf("ListEntries after the bound = %+v, %v; want doc.md", entries, err)
	}
	if isDir, err := reader.IsDir("/bound/"); err != nil || !isDir {
		t.Errorf("IsDir after the bound = %v, %v; want a directory", isDir, err)
	}
	if path, err := reader.LookupHash(storefmt.ContentHash([]byte("# Bound\n"))); err != nil || path != "/bound/doc.md" {
		t.Errorf("LookupHash of the body after the bound = %q, %v; want the document", path, err)
	}
}

func testReplicaOwnWritesAtOnce(t *testing.T, site ReplicaSite) {
	writer, _ := replicaPair(t, site)
	if _, err := writer.WriteVersion("/own.md", 0, []byte("# Own\n"), map[string]string{"tags": "ownwrite"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	results, err := writer.Lookup("ownwrite", catalog.Options{})
	if err != nil || len(results) != 1 {
		t.Errorf("writer Lookup right after its write = %+v, %v; want the document", results, err)
	}
}

func testReplicaOneSnapshotPerView(t *testing.T, site ReplicaSite) {
	writer, reader := replicaPair(t, site)
	ctx := context.Background()
	view, err := reader.OpenReadView(ctx)
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Errorf("close view: %v", err)
		}
	}()
	if _, err := view.IsDir(ctx, "/"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	mustWrite(t, writer, "/later.md", 0, "# Later\n")
	if _, err := view.Get(ctx, "/later.md", 0); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Get of a later write on a pinned view = %v, want not found", err)
	}
}
