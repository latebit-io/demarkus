package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	pathpkg "path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

var _ backend.Store = (*Store)(nil)

func TestSnapshotRefresh(t *testing.T) {
	t.Run("an unchanged tip costs one probe", func(t *testing.T) {
		memory := initializedMemory(t)
		commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "# A\n")})
		observed := newObservedBlobStore(memory)
		store, err := Open(context.Background(), observed, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		cached := store.served.Load().snap
		observed.reset()

		if entries, err := store.ListEntries("/docs", false); err != nil || len(entries) != 1 {
			t.Fatalf("catalog read = (%v, %v)", entries, err)
		}
		if counts := observed.counts(); sumCounts(counts.gets)+sumCounts(counts.heads) != 0 {
			t.Errorf("a catalog read right after open read the bucket: %v %v", counts.gets, counts.heads)
		}
		if _, err := store.Versions("/docs/a.md"); err != nil {
			t.Fatalf("path read: %v", err)
		}
		counts := observed.counts()
		if counts.gets[slotKey(cached.Sequence+1)] != 1 || len(counts.heads) != 0 || counts.gets[markerKey] != 0 {
			t.Errorf("path read operations = heads %v gets %v, want one probe of the next slot", counts.heads, counts.gets)
		}
		if store.served.Load().snap != cached {
			t.Error("an unchanged tip replaced the snapshot")
		}
	})

	t.Run("new slots apply in order, each read once", func(t *testing.T) {
		memory := initializedMemory(t)
		observed := newObservedBlobStore(memory)
		store, err := Open(context.Background(), observed, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		peer := (&bucketSite{objects: memory}).open(t, 0)
		for index, path := range []string{"/docs/one.md", "/docs/two.md", "/docs/one.md"} {
			if _, err := peer.WriteVersion(path, -1, fmt.Appendf(nil, "# %s %d\n", path, index), nil); err != nil {
				t.Fatal(err)
			}
		}
		observed.reset()
		document, err := store.Get("/docs/one.md", 0)
		if err != nil || document.Version != 2 {
			t.Fatalf("Get = (%+v, %v), want the peer's v2", document, err)
		}
		// The probe finds slot 2; a listing names 3 and 4 and confirms the tip.
		counts := observed.counts()
		for first := int64(2); first <= 4; first++ {
			if counts.gets[slotKey(first)] != 1 {
				t.Errorf("slot %d read %d times, want once", first, counts.gets[slotKey(first)])
			}
		}
		if counts.gets[slotKey(5)] != 0 {
			t.Errorf("past the listed tip probed %d times, want none", counts.gets[slotKey(5)])
		}
		if got := store.servedSequence(); got != 4 {
			t.Errorf("served sequence = %d, want 4", got)
		}
	})

	t.Run("a corrupt slot never serves stale data", func(t *testing.T) {
		memory := initializedMemory(t)
		store, err := Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		cached := store.served.Load().snap
		peer := (&bucketSite{objects: memory}).open(t, 0)
		if _, err := peer.WriteVersion("/docs/a.md", 0, []byte("# A\n"), nil); err != nil {
			t.Fatal(err)
		}
		corruptObject(t, memory, slotKey(2))
		if _, err := store.Get("/docs/a.md", 0); !errors.Is(err, blob.ErrIntegrity) || !errors.Is(err, storefmt.ErrIntegrity) {
			t.Errorf("Get over a corrupt slot = %v, want integrity", err)
		}
		if store.served.Load().snap != cached {
			t.Error("a failed refresh replaced the snapshot")
		}
		if _, err := store.WriteVersion("/docs/b.md", 0, []byte("# B\n"), nil); !errors.Is(err, blob.ErrIntegrity) {
			t.Errorf("write over a corrupt slot = %v, want integrity", err)
		}
	})

	t.Run("a catalog read trusts a tip confirmed within the bound", func(t *testing.T) {
		memory := initializedMemory(t)
		observed := newObservedBlobStore(memory)
		store, err := Open(context.Background(), observed, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		opened := time.Now()
		store.now = func() time.Time { return opened.Add(backend.SearchFreshness / 2) }
		observed.reset()
		if _, err := store.Lookup("anything", catalog.Options{}); err != nil {
			t.Fatal(err)
		}
		if n := sumCounts(observed.counts().gets); n != 0 {
			t.Errorf("a catalog read inside the bound read the bucket %d times", n)
		}
		store.now = func() time.Time { return opened.Add(2 * backend.SearchFreshness) }
		if _, err := store.Lookup("anything", catalog.Options{}); err != nil {
			t.Fatal(err)
		}
		if n := observed.counts().gets[slotKey(2)]; n != 1 {
			t.Errorf("a catalog read past the bound probed %d times, want 1", n)
		}
	})

	t.Run("a refresh never takes a tip confirmed before it asked", func(t *testing.T) {
		memory := initializedMemory(t)
		stalled := &stalledProbeStore{Store: memory, probed: make(chan struct{}), release: make(chan struct{})}
		store, err := Open(context.Background(), stalled, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		stalled.armed.Store(true)
		first := make(chan error, 1)
		go func() {
			_, err := store.refresh(context.Background())
			first <- err
		}()
		waitForTestSignal(t, stalled.probed, "first probe")
		peer := (&bucketSite{objects: memory}).open(t, 0)
		if _, err := peer.WriteVersion("/docs/late.md", 0, []byte("# Late\n"), nil); err != nil {
			t.Fatal(err)
		}
		second := make(chan *snapshot, 1)
		go func() {
			snap, err := store.refresh(context.Background())
			if err != nil {
				t.Errorf("second refresh: %v", err)
			}
			second <- snap
		}()
		time.Sleep(10 * time.Millisecond) // the second refresh waits on the first
		close(stalled.release)
		if err := <-first; err != nil {
			t.Fatalf("first refresh: %v", err)
		}
		if snap := <-second; snap.path("/docs/late.md") == nil {
			t.Error("a refresh that asked after a write took a tip confirmed before it")
		}
	})
}

// stalledProbeStore answers the first slot probe at once from the bucket but
// holds the answer until released, as a slow response does.
type stalledProbeStore struct {
	blob.Store
	armed   atomic.Bool
	probed  chan struct{}
	release chan struct{}
}

func (store *stalledProbeStore) Get(ctx context.Context, key string) (blob.Object, error) {
	object, err := store.Store.Get(ctx, key)
	if isSlot(key) && store.armed.CompareAndSwap(true, false) {
		close(store.probed)
		<-store.release
	}
	return object, err
}

func TestReadViewContextAndSnapshot(t *testing.T) {
	memory := initializedMemory(t)
	first := newReadDocument("/docs/a.md", "# A v1\n")
	first.Metadata[0] = readMetadata("A", "before", "context")
	commitReadDocuments(t, memory, []readDocumentSpec{first})
	observed := newObservedBlobStore(memory)
	store, err := Open(context.Background(), observed, Options{Logger: discardLogger, WorldID: testWorldID, RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	old, err := store.openReadView()
	if err != nil {
		t.Fatalf("open old view: %v", err)
	}
	if _, err := old.Versions("/docs/a.md"); err != nil {
		t.Fatalf("pin old view: %v", err)
	}
	contractView, err := store.OpenReadView(context.Background())
	if err != nil {
		t.Fatalf("open contract view: %v", err)
	}
	deadline := contractView.(*snapshotView).deadline
	if time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second {
		t.Fatalf("view deadline = %v, want active two-second bound", deadline)
	}
	observed.reset()

	if _, err := store.WriteVersion("/docs/a.md", 1, []byte("# A v2\n"), readMetadata("A", "after", "context")); err != nil {
		t.Fatalf("write v2 over the checkpoint's v1: %v", err)
	}
	if _, err := store.WriteVersion("/docs/new.md", 0, []byte("# New\n"), defaultReadMetadata("/docs/new.md")); err != nil {
		t.Fatalf("write new: %v", err)
	}
	observed.reset()
	assertPinnedReadView(t, old, &pinnedReadWant{
		body:          "# A v1\n",
		version:       1,
		versions:      []int{1},
		entries:       []storefmt.DirEntry{{Name: "a.md"}},
		lookup:        "before",
		missingLookup: "after",
		missingBody:   "# A v2\n",
	})
	for _, used := range observed.counts().contexts {
		if used.Value(pinnedKey{}) == nil {
			t.Error("view operation did not run under the call's context")
		}
	}

	fresh, err := store.openReadView()
	if err != nil {
		t.Fatalf("open fresh view: %v", err)
	}
	assertPinnedReadView(t, fresh, &pinnedReadWant{
		body:          "# A v2\n",
		version:       2,
		versions:      []int{2, 1},
		entries:       []storefmt.DirEntry{{Name: "a.md"}, {Name: "new.md"}},
		lookup:        "after",
		missingLookup: "before",
		missingBody:   "# A v1\n",
	})
	closeTestReadView(t, fresh)
	if err := fresh.VerifyChain("/docs/a.md"); !errors.Is(err, backend.ErrViewClosed) {
		t.Errorf("read after close = %v, want ErrViewClosed", err)
	}
	if err := store.VerifyChain("/docs/a.md"); err != nil {
		t.Errorf("chain across the checkpoint and the log: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close old: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("second close old: %v", err)
	}
	if err := contractView.Close(); err != nil {
		t.Fatalf("close contract view: %v", err)
	}
	if _, err := contractView.IsDir(context.Background(), "/"); !errors.Is(err, backend.ErrViewClosed) {
		t.Errorf("read after close error = %v, want ErrViewClosed", err)
	}
}

func TestReadSemantics(t *testing.T) {
	memory := initializedMemory(t)
	live := newReadDocument("/docs/live.md", "# Live v1\n", "# Live v2\n")
	live.Metadata = []map[string]string{
		readMetadata("Live", "before,common", "alpha"),
		readMetadata("Live", "after,common", "alpha"),
	}
	archived := newReadDocument("/docs/archived.md", "# Archived\n")
	archived.Archived = true
	archived.Metadata[0] = readMetadata("Archived", "archived", "alpha")
	archiveOnly := newReadDocument("/archive/only.md", "# Only\n")
	archiveOnly.Archived = true
	sharedA := newReadDocument("/docs/shared-a.md", "same")
	sharedZ := newReadDocument("/docs/shared-z.md", "same")
	pruned := newReadDocument("/docs/pruned.md", "one", "two", "three")
	pruned.RetainFrom = 3
	documents := []readDocumentSpec{
		live,
		archived,
		archiveOnly,
		sharedZ,
		sharedA,
		pruned,
		newReadDocument("/.secret.md", "secret"),
		newReadDocument("/work/.scratch.md", "scratch"),
		newReadDocument("/hidden/.nested/doc.md", "hidden"),
		newReadDocument("/versions/trap.md", "trap"),
	}
	commit := commitReadDocuments(t, memory, documents)
	observed := newObservedBlobStore(memory)
	store, err := Open(context.Background(), observed, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	view, err := store.openReadView()
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	defer closeTestReadView(t, view)

	t.Run("Get current historical and archived", func(t *testing.T) {
		document, err := view.Get("docs//live.md/", 0)
		if err != nil {
			t.Fatalf("get current: %v", err)
		}
		wantModified := commit.documents[live.Path].versions[2].modified
		wantRaw := commit.documents[live.Path].versions[2].raw
		if document.Version != 2 || !bytes.Equal(document.Content, []byte("# Live v2\n")) || document.Archived {
			t.Errorf("current document = %+v", document)
		}
		if !document.Modified.Equal(wantModified) || document.Modified.Location() != time.UTC || document.Modified.Nanosecond() != 0 {
			t.Errorf("modified = %v, want UTC seconds %v", document.Modified, wantModified)
		}
		if document.ETag != storefmt.StoredETag(wantRaw) || document.Metadata["tags"] != "after,common" {
			t.Errorf("etag/meta = %q %v", document.ETag, document.Metadata)
		}
		document.Content[0] = 'X'
		document.Metadata["tags"] = "mutated"
		again, err := view.Get(live.Path, 0)
		if err != nil || !bytes.Equal(again.Content, []byte("# Live v2\n")) || again.Metadata["tags"] != "after,common" {
			t.Errorf("Get after caller mutation = (%+v, %v)", again, err)
		}

		historical, err := view.Get(live.Path, 1)
		if err != nil {
			t.Fatalf("get historical: %v", err)
		}
		if historical.Version != 1 || historical.Archived || historical.Metadata["tags"] != "before,common" || !bytes.Equal(historical.Content, []byte("# Live v1\n")) {
			t.Errorf("historical document = %+v", historical)
		}
		archivedDocument, err := view.Get(archived.Path, 0)
		if err != nil || !archivedDocument.Archived || archivedDocument.Version != 1 {
			t.Errorf("archived Get = (%+v, %v)", archivedDocument, err)
		}
		pinnedArchived, err := view.Get(archived.Path, 1)
		if err != nil || pinnedArchived.Archived || pinnedArchived.ETag != archivedDocument.ETag {
			t.Errorf("pinned archived Get = (%+v, %v), current ETag %q", pinnedArchived, err, archivedDocument.ETag)
		}
	})

	t.Run("logical misses", func(t *testing.T) {
		tests := []struct {
			name    string
			path    string
			version int
		}{
			{name: "document", path: "/missing.md"},
			{name: "version", path: live.Path, version: 9},
			{name: "negative version", path: live.Path, version: -1},
			{name: "pruned version", path: pruned.Path, version: 1},
			{name: "dot dot", path: "/docs/../live.md"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if _, err := view.Get(test.path, test.version); !errors.Is(err, backend.ErrNotFound) {
					t.Errorf("Get() error = %v, want not-exist", err)
				}
			})
		}
		if _, err := store.Get("/missing.md", 0); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("direct Get error = %v, want exact backend.ErrNotFound", err)
		}
	})

	t.Run("Versions uses history only", func(t *testing.T) {
		observed.reset()
		versions, err := view.Versions(live.Path)
		if err != nil {
			t.Fatalf("versions: %v", err)
		}
		got := make([]int, len(versions))
		for index := range versions {
			got[index] = versions[index].Version
		}
		if !slices.Equal(got, []int{2, 1}) {
			t.Errorf("versions = %v, want [2 1]", got)
		}
		if countPrefix(observed.counts().gets, objectPrefix+"blobs/") != 0 {
			t.Error("Versions loaded a blob")
		}
		prunedVersions, err := view.Versions(pruned.Path)
		if err != nil || len(prunedVersions) != 1 || prunedVersions[0].Version != 3 {
			t.Errorf("pruned versions = (%v, %v)", prunedVersions, err)
		}
		if _, err := view.Versions("/missing.md"); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("missing Versions error = %v", err)
		}
	})

	t.Run("chain", func(t *testing.T) {
		if err := view.VerifyChain(live.Path); err != nil {
			t.Errorf("live chain: %v", err)
		}
		if err := view.VerifyChain(pruned.Path); err != nil {
			t.Errorf("pruned chain: %v", err)
		}
		if err := view.VerifyChain("/missing.md"); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("missing chain error = %v", err)
		}
	})

	t.Run("directory topology", func(t *testing.T) {
		assertEntries(t, view, "/", false, []storefmt.DirEntry{{Name: "docs", IsDir: true}})
		assertEntries(t, view, "/", true, []storefmt.DirEntry{{Name: "archive", IsDir: true}, {Name: "docs", IsDir: true}})
		assertEntries(t, view, "/docs", false, []storefmt.DirEntry{
			{Name: "live.md"},
			{Name: "pruned.md"},
			{Name: "shared-a.md"},
			{Name: "shared-z.md"},
		})
		assertEntries(t, view, "/archive", false, []storefmt.DirEntry{})
		if _, err := view.ListEntries(live.Path, false); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("list document error = %v", err)
		}
		if _, err := view.ListEntries("/missing", false); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("list missing error = %v", err)
		}
		if _, err := view.ListEntries("/../docs", false); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("list dot-dot error = %v", err)
		}

		checks := []struct {
			path    string
			want    bool
			wantErr bool
		}{
			{path: "/", want: true},
			{path: "/docs", want: true},
			{path: "/work", want: true},
			{path: live.Path},
			{path: "/missing", wantErr: true},
			{path: "/../docs", wantErr: true},
		}
		for _, check := range checks {
			isDirectory, err := view.IsDir(check.path)
			if isDirectory != check.want || errors.Is(err, backend.ErrNotFound) != check.wantErr {
				t.Errorf("IsDir(%q) = (%v, %v), want (%v, not-exist=%v)", check.path, isDirectory, err, check.want, check.wantErr)
			}
		}
	})

	t.Run("hash and catalog", func(t *testing.T) {
		sharedHash := storefmt.ContentHash([]byte("same"))
		path, err := view.LookupHash(sharedHash)
		if err != nil || path != sharedA.Path {
			t.Errorf("LookupHash = (%q, %v), want %q", path, err, sharedA.Path)
		}
		if _, err := view.LookupHash(storefmt.ContentHash([]byte("# Archived\n"))); !errors.Is(err, backend.ErrNotFound) {
			t.Errorf("archived hash error = %v", err)
		}
		results, err := view.Lookup("after", catalog.Options{Scope: "/docs"})
		if err != nil || len(results) != 1 || results[0].Path != live.Path {
			t.Fatalf("Lookup(after) = (%+v, %v)", results, err)
		}
		results[0].Tags[0] = "mutated"
		results[0].Metadata["project"] = "mutated"
		again, err := view.Lookup("after", catalog.Options{Scope: "/docs"})
		if err != nil || again[0].Tags[0] == "mutated" || again[0].Metadata["project"] != "alpha" {
			t.Errorf("Lookup after caller mutation = (%+v, %v)", again, err)
		}
		archivedResults, err := view.Lookup("archived", catalog.Options{})
		if err != nil || archivedResults != nil {
			t.Errorf("archived Lookup = (%+v, %v)", archivedResults, err)
		}
	})

	t.Run("current version", func(t *testing.T) {
		version, err := store.CurrentVersionResult(live.Path)
		if err != nil || version != 2 {
			t.Errorf("CurrentVersion = (%d, %v)", version, err)
		}
		if got, err := store.CurrentVersionResult("/missing.md"); got != 0 || err != nil {
			t.Errorf("missing CurrentVersion = (%d, %v), want (0, nil)", got, err)
		}
	})
}

func TestReferencedObjectIntegrity(t *testing.T) {
	tests := []struct {
		name      string
		version   int
		wantCause error
		mutate    func(*testing.T, *blob.Memory, *readCommit)
	}{
		{
			name:      "missing manifest",
			wantCause: blob.ErrNotFound,
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				deleteObject(t, memory, commit.documents["/docs/a.md"].entry.Manifest.Key)
			},
		},
		{
			name: "corrupt manifest",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				corruptObject(t, memory, commit.documents["/docs/a.md"].entry.Manifest.Key)
			},
		},
		{
			name: "manifest path hash mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				installManifestMutation(t, memory, commit.documents["/docs/a.md"], func(manifest *manifestObject) {
					manifest.PathHash = strings.Repeat("a", 64)
					for index := range manifest.History {
						manifest.History[index].PathHash = manifest.PathHash
					}
				})
			},
		},
		{
			name: "manifest current mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				installManifestMutation(t, memory, commit.documents["/docs/a.md"], func(manifest *manifestObject) {
					manifest.Current = 1
					manifest.History[0].Last = 1
				})
			},
		},
		{
			name: "manifest archive mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				installManifestMutation(t, memory, commit.documents["/docs/a.md"], func(manifest *manifestObject) {
					manifest.Archived = true
				})
			},
		},
		{
			name:      "missing history",
			wantCause: blob.ErrNotFound,
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				deleteObject(t, memory, commit.documents["/docs/a.md"].historyRef.Key)
			},
		},
		{
			name: "corrupt history",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				corruptObject(t, memory, commit.documents["/docs/a.md"].historyRef.Key)
			},
		},
		{
			name: "history path hash mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				history := document.history
				history.PathHash = strings.Repeat("b", 64)
				installHistoryMutation(t, memory, document, history, false)
			},
		},
		{
			name: "history range mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				history := document.history
				history.First = 2
				history.Entries = slices.Clone(history.Entries[1:])
				installHistoryMutation(t, memory, document, history, false)
			},
		},
		{
			name:      "missing blob",
			wantCause: blob.ErrNotFound,
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				deleteObject(t, memory, commit.documents["/docs/a.md"].versions[2].entry.Blob.Key)
			},
		},
		{
			name: "corrupt blob",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				corruptObject(t, memory, commit.documents["/docs/a.md"].versions[2].entry.Blob.Key)
			},
		},
		{
			name:    "malformed stored version",
			version: 1,
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				raw := bytes.Replace(document.versions[1].raw, []byte("version: 1\n"), []byte("version: 01\n"), 1)
				installRawMutation(t, memory, document, 1, raw)
			},
		},
		{
			name: "tip body hash mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				entry := document.entry
				entry.BodyHash = storefmt.ContentHash([]byte("other"))
				installSnapshotEntry(t, memory, &entry)
			},
		},
		{
			name: "tip modified mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				entry := document.entry
				entry.Modified = "2026-08-22T13:00:00Z"
				entry.Catalog.Modified = entry.Modified
				installSnapshotEntry(t, memory, &entry)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := initializedMemory(t)
			commit := commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "# A v1\n", "# A v2\n")})
			test.mutate(t, memory, &commit)
			store, err := Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			view, err := store.openReadView()
			if err != nil {
				t.Fatalf("open view: %v", err)
			}
			defer closeTestReadView(t, view)
			_, err = view.Get("/docs/a.md", test.version)
			if !errors.Is(err, blob.ErrIntegrity) || !errors.Is(err, storefmt.ErrIntegrity) {
				t.Fatalf("Get error = %v, want integrity", err)
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Errorf("Get error = %v, want provider cause %v", err, test.wantCause)
			}
		})
	}
}

func TestStoredArchiveHeaderIsLegacyOnly(t *testing.T) {
	memory := initializedMemory(t)
	document := newReadDocument("/docs/a.md", "one", "two")
	document.StoredArchived = map[int]bool{1: true, 2: true}
	commit := commitReadDocuments(t, memory, []readDocumentSpec{document})
	view := openTestReadView(t, memory)
	defer closeTestReadView(t, view)

	for _, version := range []int{0, 1, 2} {
		stored, err := view.Get(document.Path, version)
		if err != nil {
			t.Fatalf("Get(v%d): %v", version, err)
		}
		if stored.Archived {
			t.Errorf("Get(v%d) archived = true, want model state false", version)
		}
		resolved := version
		if resolved == 0 {
			resolved = 2
		}
		wantETag := storefmt.StoredETag(commit.documents[document.Path].versions[resolved].raw)
		if stored.ETag != wantETag {
			t.Errorf("Get(v%d) ETag = %q, want %q", version, stored.ETag, wantETag)
		}
	}
	if err := view.VerifyChain(document.Path); err != nil {
		t.Fatalf("VerifyChain with legacy archive headers: %v", err)
	}
}

func TestReadIntegrityNormalization(t *testing.T) {
	type readSurface interface {
		Get(string, int) (*storefmt.Document, error)
		Versions(string) ([]storefmt.VersionInfo, error)
		VerifyChain(string) error
	}
	operations := []struct {
		name string
		read func(readSurface) error
	}{
		{name: "Get", read: func(surface readSurface) error {
			_, err := surface.Get("/docs/a.md", 0)
			return err
		}},
		{name: "Versions", read: func(surface readSurface) error {
			_, err := surface.Versions("/docs/a.md")
			return err
		}},
		{name: "VerifyChain", read: func(surface readSurface) error {
			return surface.VerifyChain("/docs/a.md")
		}},
	}
	for _, surfaceName := range []string{"direct", "request view"} {
		for _, operation := range operations {
			t.Run(surfaceName+" "+operation.name, func(t *testing.T) {
				memory := initializedMemory(t)
				commit := commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "# A\n")})
				store, err := Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID})
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				var surface readSurface = store
				var view backend.ReadView
				if surfaceName == "request view" {
					view, err = store.OpenReadView(context.Background())
					if err != nil {
						t.Fatalf("open request view: %v", err)
					}
					defer func() {
						if err := view.Close(); err != nil {
							t.Errorf("close request view: %v", err)
						}
					}()
					surface = &pinnedView{ctx: context.Background(), view: view}
				}
				deleteObject(t, memory, commit.documents["/docs/a.md"].entry.Manifest.Key)
				err = operation.read(surface)
				if !errors.Is(err, storefmt.ErrIntegrity) || !errors.Is(err, blob.ErrIntegrity) || !errors.Is(err, blob.ErrNotFound) {
					t.Fatalf("read error = %v, want protocol integrity with blob integrity and not-found causes", err)
				}
			})
		}
	}
}

func TestVerifyChainErrors(t *testing.T) {
	t.Run("missing previous hash", func(t *testing.T) {
		memory := initializedMemory(t)
		commit := commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "one", "two")})
		document := commit.documents["/docs/a.md"]
		raw := removePreviousHash(t, document.versions[2].raw)
		installRawMutation(t, memory, document, 2, raw)
		view := openTestReadView(t, memory)
		defer closeTestReadView(t, view)
		err := view.VerifyChain("/docs/a.md")
		if !errors.Is(err, blob.ErrIntegrity) || !strings.Contains(err.Error(), "v2 missing previous-hash") {
			t.Errorf("VerifyChain error = %v", err)
		}
	})

	t.Run("chain broken", func(t *testing.T) {
		memory := initializedMemory(t)
		commit := commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "one", "two")})
		document := commit.documents["/docs/a.md"]
		raw, err := storefmt.SerializeVersion(1, nil, []byte("changed"), document.versions[1].metadata)
		if err != nil {
			t.Fatalf("serialize changed v1: %v", err)
		}
		installRawMutation(t, memory, document, 1, raw)
		view := openTestReadView(t, memory)
		defer closeTestReadView(t, view)
		err = view.VerifyChain("/docs/a.md")
		if !errors.Is(err, blob.ErrIntegrity) || !strings.Contains(err.Error(), "v2 chain broken") {
			t.Errorf("VerifyChain error = %v", err)
		}
	})

	t.Run("loads blobs oldest first", func(t *testing.T) {
		memory := initializedMemory(t)
		commit := commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "one", "two", "three")})
		observed := newObservedBlobStore(memory)
		store, err := Open(context.Background(), observed, Options{Logger: discardLogger, WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		view, err := store.openReadView()
		if err != nil {
			t.Fatalf("open view: %v", err)
		}
		defer closeTestReadView(t, view)
		observed.reset()
		if err := view.VerifyChain("/docs/a.md"); err != nil {
			t.Fatalf("VerifyChain: %v", err)
		}
		var blobGets []string
		for _, key := range observed.counts().getOrder {
			if strings.HasPrefix(key, objectPrefix+"blobs/") {
				blobGets = append(blobGets, key)
			}
		}
		want := []string{
			commit.documents["/docs/a.md"].versions[1].entry.Blob.Key,
			commit.documents["/docs/a.md"].versions[2].entry.Blob.Key,
			commit.documents["/docs/a.md"].versions[3].entry.Blob.Key,
		}
		if !slices.Equal(blobGets, want) {
			t.Errorf("blob read order = %v, want %v", blobGets, want)
		}
	})
}

func TestReadViewTimeout(t *testing.T) {
	memory := initializedMemory(t)
	commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "# A\n")})
	store, err := Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Tighten after the open so a slow runner cannot time out the open itself.
	store.requestTimeout = 25 * time.Millisecond
	store.objects = &blockingGetStore{Store: memory, prefix: logPrefix}
	view, err := store.OpenReadView(context.Background())
	if err != nil {
		t.Fatalf("OpenReadView() = %v; the view pins at its first read", err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	if _, err := view.Get(context.Background(), "/docs/a.md", 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("first read = %v, want the view's deadline", err)
	}
}

type readDocumentSpec struct {
	Path           string
	Bodies         [][]byte
	Metadata       []map[string]string
	Archived       bool
	StoredArchived map[int]bool
	RetainFrom     int
}

type committedReadVersion struct {
	raw      []byte
	metadata map[string]string
	modified time.Time
	entry    historyEntry
}

type committedReadDocument struct {
	entry      shardEntry
	manifest   manifestObject
	history    historyObject
	historyRef historyRef
	versions   map[int]committedReadVersion
}

type readCommit struct {
	documents map[string]*committedReadDocument
	root      rootObject
	rootRef   objectRef
}

func newReadDocument(path string, bodies ...string) readDocumentSpec {
	spec := readDocumentSpec{
		Path:       path,
		Bodies:     make([][]byte, len(bodies)),
		Metadata:   make([]map[string]string, len(bodies)),
		RetainFrom: 1,
	}
	for index, body := range bodies {
		spec.Bodies[index] = []byte(body)
		spec.Metadata[index] = defaultReadMetadata(path)
	}
	return spec
}

func defaultReadMetadata(documentPath string) map[string]string {
	return readMetadata(pathpkg.Base(documentPath), "read,test", "default")
}

func readMetadata(title, tags, project string) map[string]string {
	return map[string]string{
		"importance": "0.7",
		"project":    project,
		"tags":       tags,
		"title":      title,
		"type":       "Document",
	}
}

func commitReadDocuments(t *testing.T, memory *blob.Memory, specs []readDocumentSpec) readCommit {
	t.Helper()
	commit := readCommit{documents: make(map[string]*committedReadDocument, len(specs))}
	grouped := make(map[int][]shardEntry)
	for _, spec := range specs {
		document := buildReadDocument(t, memory, &spec)
		if _, exists := commit.documents[spec.Path]; exists {
			t.Fatalf("duplicate test document %q", spec.Path)
		}
		commit.documents[spec.Path] = document
		index := shardIndexForPath(t, spec.Path)
		grouped[index] = append(grouped[index], document.entry)
	}

	refs := make([]shardRef, shardCount)
	for index := range shardCount {
		entries := grouped[index]
		if entries == nil {
			entries = make([]shardEntry, 0)
		}
		sort.Slice(entries, func(left, right int) bool {
			return entries[left].Path < entries[right].Path
		})
		shardID := fmt.Sprintf("%02x", index)
		shard := shardObject{Schema: schemaVersion, Shard: shardID, Entries: entries}
		model, ref, err := immutableJSON(func(hash string) string { return shardKey(shardID, hash) }, shard)
		if err != nil {
			t.Fatalf("build shard %s: %v", shardID, err)
		}
		createReadObject(t, memory, model)
		refs[index] = shardRef{Shard: shardID, objectRef: ref}
	}
	root := rootObject{
		Schema:        schemaVersion,
		WorldID:       testWorldID,
		DocumentCount: len(specs),
		Shards:        refs,
	}
	_, rootRef, err := immutableJSON(rootKey, root)
	if err != nil {
		t.Fatalf("build root ref: %v", err)
	}
	installRoot(t, memory, root)
	commit.root = root
	commit.rootRef = rootRef
	return commit
}

func buildReadDocument(t *testing.T, memory *blob.Memory, spec *readDocumentSpec) *committedReadDocument {
	t.Helper()
	if len(spec.Bodies) == 0 || len(spec.Metadata) != len(spec.Bodies) {
		t.Fatalf("invalid test document %q", spec.Path)
	}
	if spec.RetainFrom == 0 {
		spec.RetainFrom = 1
	}
	if spec.RetainFrom < 1 || spec.RetainFrom > len(spec.Bodies) {
		t.Fatalf("invalid retain-from %d for %q", spec.RetainFrom, spec.Path)
	}

	pathSum := pathHash(spec.Path)
	document := &committedReadDocument{versions: make(map[int]committedReadVersion, len(spec.Bodies))}
	var previousRaw []byte
	retained := make([]historyEntry, 0, len(spec.Bodies)-spec.RetainFrom+1)
	for index, body := range spec.Bodies {
		version := index + 1
		raw, err := storefmt.SerializeVersion(version, previousRaw, body, spec.Metadata[index])
		if err != nil {
			t.Fatalf("serialize %s v%d: %v", spec.Path, version, err)
		}
		if spec.StoredArchived[version] {
			raw, err = storefmt.SetArchived(raw, true)
			if err != nil {
				t.Fatalf("archive %s v%d: %v", spec.Path, version, err)
			}
		}
		previousRaw = raw
		modified := readVersionModified(version)
		entry := historyEntry{
			Version:  version,
			BodyHash: storefmt.ContentHash(body),
			Modified: modified.Format(time.RFC3339),
		}
		if version >= spec.RetainFrom {
			blobHash := hashHex(raw)
			entry.Blob = objectRef{Key: blobKey(blobHash), Hash: blobHash}
			createReadObject(t, memory, modelObject{Key: entry.Blob.Key, Data: raw})
			retained = append(retained, entry)
		}
		document.versions[version] = committedReadVersion{
			raw:      bytes.Clone(raw),
			metadata: maps.Clone(spec.Metadata[index]),
			modified: modified,
			entry:    entry,
		}
	}

	history := historyObject{
		Schema:   schemaVersion,
		PathHash: pathSum,
		First:    spec.RetainFrom,
		Last:     len(spec.Bodies),
		Entries:  retained,
	}
	historyModel, historyObjectRef, err := immutableJSON(historyKey, history)
	if err != nil {
		t.Fatalf("build history %q: %v", spec.Path, err)
	}
	createReadObject(t, memory, historyModel)
	historyReference := historyRef{
		PathHash:  pathSum,
		First:     history.First,
		Last:      history.Last,
		objectRef: historyObjectRef,
	}
	manifest := manifestObject{
		Schema:   schemaVersion,
		PathHash: pathSum,
		Current:  len(spec.Bodies),
		Archived: spec.Archived,
		History:  []historyRef{historyReference},
	}
	manifestModel, manifestRef, err := immutableJSON(func(hash string) string {
		return manifestKey(pathSum, hash)
	}, manifest)
	if err != nil {
		t.Fatalf("build manifest %q: %v", spec.Path, err)
	}
	createReadObject(t, memory, manifestModel)

	tip := document.versions[len(spec.Bodies)]
	metadata := storefmt.ExtractMetadata(tip.raw)
	catalogEntry := catalog.FromDocument(spec.Path, metadata, spec.Bodies[len(spec.Bodies)-1], tip.modified)
	tags := slices.Clone(catalogEntry.Tags)
	if tags == nil {
		tags = make([]string, 0)
	}
	document.entry = shardEntry{
		Path:     spec.Path,
		PathHash: pathSum,
		Manifest: manifestRef,
		Current:  len(spec.Bodies),
		Archived: spec.Archived,
		BodyHash: tip.entry.BodyHash,
		Modified: tip.entry.Modified,
		Catalog: catalogRecord{
			Path:       spec.Path,
			Title:      catalogEntry.Title,
			Tags:       tags,
			Importance: strconv.FormatFloat(catalogEntry.Importance, 'f', -1, 64),
			Modified:   tip.entry.Modified,
			Metadata:   maps.Clone(catalogEntry.Metadata),
		},
	}
	document.manifest = manifest
	document.history = history
	document.historyRef = historyReference
	return document
}

func readVersionModified(version int) time.Time {
	return time.Date(2026, 8, 22, 12, 34, 50+version, 0, time.UTC)
}

func createReadObject(t *testing.T, memory blob.Store, object modelObject) {
	t.Helper()
	if err := createImmutable(context.Background(), memory, object); err != nil {
		t.Fatalf("create test object %q: %v", object.Key, err)
	}
}

func installManifestMutation(
	t *testing.T,
	memory *blob.Memory,
	document *committedReadDocument,
	mutate func(*manifestObject),
) {
	t.Helper()
	manifest := document.manifest
	manifest.History = slices.Clone(manifest.History)
	mutate(&manifest)
	installManifestObject(t, memory, document, manifest)
}

func installHistoryMutation(
	t *testing.T,
	memory *blob.Memory,
	document *committedReadDocument,
	history historyObject,
	matchReference bool,
) {
	t.Helper()
	if err := validateHistoryObject(&history); err != nil {
		t.Fatalf("invalid mutated history: %v", err)
	}
	model, ref, err := immutableJSON(historyKey, history)
	if err != nil {
		t.Fatalf("build mutated history: %v", err)
	}
	createReadObject(t, memory, model)
	manifest := document.manifest
	manifest.History = slices.Clone(manifest.History)
	manifest.History[0].objectRef = ref
	if matchReference {
		manifest.History[0].PathHash = history.PathHash
		manifest.History[0].First = history.First
		manifest.History[0].Last = history.Last
	}
	installManifestObject(t, memory, document, manifest)
}

func installRawMutation(
	t *testing.T,
	memory *blob.Memory,
	document *committedReadDocument,
	version int,
	raw []byte,
) {
	t.Helper()
	history := document.history
	history.Entries = slices.Clone(history.Entries)
	index := version - history.First
	if index < 0 || index >= len(history.Entries) {
		t.Fatalf("version %d is outside retained history", version)
	}
	blobHash := hashHex(raw)
	ref := objectRef{Key: blobKey(blobHash), Hash: blobHash}
	createReadObject(t, memory, modelObject{Key: ref.Key, Data: raw})
	history.Entries[index].Blob = ref
	history.Entries[index].BodyHash = storefmt.ContentHash(storefmt.ExtractBody(raw))
	installHistoryMutation(t, memory, document, history, true)
}

func installManifestObject(t *testing.T, memory *blob.Memory, document *committedReadDocument, manifest manifestObject) {
	t.Helper()
	if err := validateManifestObject(&manifest); err != nil {
		t.Fatalf("invalid mutated manifest: %v", err)
	}
	model, ref, err := immutableJSON(func(hash string) string {
		return manifestKey(document.entry.PathHash, hash)
	}, manifest)
	if err != nil {
		t.Fatalf("build mutated manifest: %v", err)
	}
	createReadObject(t, memory, model)
	entry := document.entry
	entry.Manifest = ref
	installSnapshotEntry(t, memory, &entry)
}

func installSnapshotEntry(t *testing.T, memory *blob.Memory, entry *shardEntry) {
	t.Helper()
	_, root := readCheckpointAndRoot(t, memory)
	index := shardIndexForPath(t, entry.Path)
	shardValue := getObject(t, memory, root.Shards[index].Key)
	var shard shardObject
	decodeObject(t, shardValue.Data, &shard)
	found := false
	for entryIndex := range shard.Entries {
		if shard.Entries[entryIndex].Path == entry.Path {
			shard.Entries[entryIndex] = *entry
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("snapshot entry %q not found", entry.Path)
	}
	shardID := fmt.Sprintf("%02x", index)
	model, ref, err := immutableJSON(func(hash string) string { return shardKey(shardID, hash) }, shard)
	if err != nil {
		t.Fatalf("build mutated shard: %v", err)
	}
	createReadObject(t, memory, model)
	root.Shards[index] = shardRef{Shard: shardID, objectRef: ref}
	installRoot(t, memory, root)
}

func corruptObject(t *testing.T, memory *blob.Memory, key string) {
	t.Helper()
	object := getObject(t, memory, key)
	replaceObject(t, memory, key, object.Attributes.Generation, []byte("corrupt"))
}

func removePreviousHash(t *testing.T, raw []byte) []byte {
	t.Helper()
	start := bytes.Index(raw, []byte("previous-hash: "))
	if start < 0 {
		t.Fatal("test raw has no previous-hash")
	}
	end := bytes.IndexByte(raw[start:], '\n')
	if end < 0 {
		t.Fatal("test previous-hash has no newline")
	}
	end += start + 1
	without := make([]byte, 0, len(raw)-(end-start))
	without = append(without, raw[:start]...)
	without = append(without, raw[end:]...)
	return without
}

func openTestReadView(t *testing.T, memory blob.Store) *pinnedView {
	t.Helper()
	store, err := Open(context.Background(), memory, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	view, err := store.openReadView()
	if err != nil {
		t.Fatalf("open test view: %v", err)
	}
	return view
}

func closeTestReadView(t *testing.T, view *pinnedView) {
	t.Helper()
	if view != nil {
		if err := view.Close(); err != nil {
			t.Errorf("close read view: %v", err)
		}
	}
}

func assertViewBody(t *testing.T, view *pinnedView, wantBody string, wantVersion int) {
	t.Helper()
	document, err := view.Get("/docs/a.md", 0)
	if err != nil {
		t.Fatalf("view Get: %v", err)
	}
	if document.Version != wantVersion || !bytes.Equal(document.Content, []byte(wantBody)) {
		t.Errorf("view document = v%d %q, want v%d %q", document.Version, document.Content, wantVersion, wantBody)
	}
}

type pinnedReadWant struct {
	body          string
	version       int
	versions      []int
	entries       []storefmt.DirEntry
	lookup        string
	missingLookup string
	missingBody   string
}

func assertPinnedReadView(t *testing.T, view *pinnedView, want *pinnedReadWant) {
	t.Helper()
	assertViewBody(t, view, want.body, want.version)
	if isDirectory, err := view.IsDir("/docs"); err != nil || !isDirectory {
		t.Errorf("IsDir(/docs) = (%v, %v)", isDirectory, err)
	}
	entries, err := view.ListEntries("/docs", false)
	if err != nil || !slices.Equal(entries, want.entries) {
		t.Errorf("ListEntries = (%+v, %v), want %+v", entries, err, want.entries)
	}
	versions, err := view.Versions("/docs/a.md")
	if err != nil {
		t.Errorf("Versions: %v", err)
	} else {
		got := make([]int, len(versions))
		for index := range versions {
			got[index] = versions[index].Version
		}
		if !slices.Equal(got, want.versions) {
			t.Errorf("Versions = %v, want %v", got, want.versions)
		}
	}
	hash := storefmt.ContentHash([]byte(want.body))
	if path, err := view.LookupHash(hash); err != nil || path != "/docs/a.md" {
		t.Errorf("LookupHash = (%q, %v)", path, err)
	}
	if _, err := view.LookupHash(storefmt.ContentHash([]byte(want.missingBody))); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("missing LookupHash error = %v", err)
	}
	results, err := view.Lookup(want.lookup, catalog.Options{Scope: "/docs"})
	if err != nil || len(results) != 1 || results[0].Path != "/docs/a.md" {
		t.Errorf("Lookup(%q) = (%+v, %v)", want.lookup, results, err)
	}
	results, err = view.Lookup(want.missingLookup, catalog.Options{Scope: "/docs"})
	if err != nil || len(results) != 0 {
		t.Errorf("Lookup(%q) = (%+v, %v), want empty", want.missingLookup, results, err)
	}
	if err := view.VerifyChain("/docs/a.md"); err != nil {
		t.Errorf("VerifyChain: %v", err)
	}
}

func assertEntries(t *testing.T, view *pinnedView, requestPath string, includeArchived bool, want []storefmt.DirEntry) {
	t.Helper()
	entries, err := view.ListEntries(requestPath, includeArchived)
	if err != nil {
		t.Fatalf("ListEntries(%q): %v", requestPath, err)
	}
	if !slices.Equal(entries, want) {
		t.Errorf("ListEntries(%q, %v) = %+v, want %+v", requestPath, includeArchived, entries, want)
	}
}

func shardIndexForPath(t *testing.T, documentPath string) int {
	t.Helper()
	value, err := strconv.ParseUint(pathHash(documentPath)[:2], 16, 8)
	if err != nil {
		t.Fatalf("parse shard for %q: %v", documentPath, err)
	}
	return int(value)
}

type observedBlobStore struct {
	blob.Store
	mu       sync.Mutex
	heads    map[string]int
	gets     map[string]int
	getOrder []string
	contexts []context.Context
}

type observedBlobCounts struct {
	heads    map[string]int
	gets     map[string]int
	getOrder []string
	contexts []context.Context
}

func newObservedBlobStore(store blob.Store) *observedBlobStore {
	observed := &observedBlobStore{Store: store}
	observed.reset()
	return observed
}

func (store *observedBlobStore) Get(ctx context.Context, key string) (blob.Object, error) {
	store.mu.Lock()
	store.gets[key]++
	store.getOrder = append(store.getOrder, key)
	store.contexts = append(store.contexts, ctx)
	store.mu.Unlock()
	return store.Store.Get(ctx, key)
}

func (store *observedBlobStore) Head(ctx context.Context, key string) (blob.Attributes, error) {
	store.mu.Lock()
	store.heads[key]++
	store.contexts = append(store.contexts, ctx)
	store.mu.Unlock()
	return store.Store.Head(ctx, key)
}

func (store *observedBlobStore) reset() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.heads = make(map[string]int)
	store.gets = make(map[string]int)
	store.getOrder = nil
	store.contexts = nil
}

func (store *observedBlobStore) counts() observedBlobCounts {
	store.mu.Lock()
	defer store.mu.Unlock()
	return observedBlobCounts{
		heads:    maps.Clone(store.heads),
		gets:     maps.Clone(store.gets),
		getOrder: slices.Clone(store.getOrder),
		contexts: slices.Clone(store.contexts),
	}
}

func sumCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

func countPrefix(counts map[string]int, prefix string) int {
	total := 0
	for key, count := range counts {
		if strings.HasPrefix(key, prefix) {
			total += count
		}
	}
	return total
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", name)
	}
}
