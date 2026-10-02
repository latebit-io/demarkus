package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
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

// RunReplicaConformance proves a store several replicas share: exact reads by
// path, catalog reads within backend.SearchFreshness, one snapshot per view,
// and no write lost to writers racing on several replicas.
func RunReplicaConformance(t *testing.T, newSite func(t *testing.T) ReplicaSite) {
	subtests := []struct {
		name string
		run  func(*testing.T, ReplicaSite)
	}{
		{"PathReadsAreExact", testReplicaPathReadsAreExact},
		{"CatalogReadsWithinBound", testReplicaCatalogReadsWithinBound},
		{"OwnWritesAtOnce", testReplicaOwnWritesAtOnce},
		{"OneSnapshotPerView", testReplicaOneSnapshotPerView},
		{"ConcurrentWritersLoseNothing", testReplicaConcurrentWriters},
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

func testReplicaConcurrentWriters(t *testing.T, site ReplicaSite) {
	stores := make([]Direct, 3)
	for index := range stores {
		stores[index] = Direct{Store: site.Open(t)}
	}
	ConcurrentWriters(t, stores, 4, Direct{Store: site.Open(t)})
}

// ConcurrentWriters races writers on each store over shared and own documents,
// then checks through checker: every acknowledged write keeps its version and
// body, each version is given once, and only a contended write is refused.
func ConcurrentWriters(t *testing.T, stores []Direct, writers int, checker Direct) {
	t.Helper()
	const rounds = 6
	shared := []string{"/race/a.md", "/race/b.md"}
	type ack struct {
		path    string
		version int
		body    string
	}
	var mu sync.Mutex
	var acks []ack
	var wg sync.WaitGroup
	for replica, store := range stores {
		for writer := range writers {
			wg.Go(func() {
				write := func(path string, expected int, body string, contended bool) {
					doc, err := store.WriteVersion(path, expected, []byte(body), nil)
					switch {
					case err == nil && doc.Version == expected+1:
						mu.Lock()
						acks = append(acks, ack{path: path, version: doc.Version, body: body})
						mu.Unlock()
					case err == nil:
						t.Errorf("write %s at expected %d acknowledged v%d", path, expected, doc.Version)
					case !contended || !errors.Is(err, storefmt.ErrConflict):
						t.Errorf("write %s at expected %d: %v", path, expected, err)
					}
				}
				for round := range rounds {
					path := shared[(writer+round)%len(shared)]
					expected, err := store.CurrentVersion(path)
					if err != nil {
						t.Errorf("read %s: %v", path, err)
						return
					}
					write(path, expected, fmt.Sprintf("# %d %d %d\n", replica, writer, round), true)
					write(fmt.Sprintf("/race/own/%d-%d-%d.md", replica, writer, round), 0, "# own\n", false)
				}
			})
		}
	}
	wg.Wait()

	given := map[string][]int{}
	for _, acked := range acks {
		doc, err := checker.Get(acked.path, acked.version)
		if err != nil || string(doc.Content) != acked.body {
			t.Errorf("acknowledged %s v%d = %+v, %v; want body %q", acked.path, acked.version, doc, err, acked.body)
		}
		given[acked.path] = append(given[acked.path], acked.version)
	}
	if own := len(acks) - len(given[shared[0]]) - len(given[shared[1]]); own != len(stores)*writers*rounds {
		t.Errorf("own-path writes acknowledged = %d, want %d", own, len(stores)*writers*rounds)
	}
	for _, path := range shared {
		current := currentVersion(t, checker, path)
		versions := given[path]
		slices.Sort(versions)
		for index, version := range versions {
			if version != index+1 {
				t.Fatalf("%s acknowledged versions %v, want each of 1..%d once", path, versions, current)
			}
		}
		if len(versions) != current {
			t.Errorf("%s is at v%d with %d acknowledged writes", path, current, len(versions))
		}
	}
}
