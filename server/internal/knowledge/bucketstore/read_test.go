package bucketstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	pathpkg "path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

var (
	_ backend.Reader        = (*Store)(nil)
	_ backend.CatalogReader = (*Store)(nil)
	_ backend.ViewProvider  = (*Store)(nil)
	_ backend.ReadView      = (*readView)(nil)
)

func TestSnapshotRefresh(t *testing.T) {
	t.Run("unchanged head uses Head and reuses snapshot", func(t *testing.T) {
		memory := initializedMemory(t)
		commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "# A\n")})
		observed := newObservedBlobStore(memory)
		store, err := Open(context.Background(), observed, Options{WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		cached := store.snapshot.Load()
		observed.reset()

		view, err := store.openReadView()
		if err != nil {
			t.Fatalf("open read view: %v", err)
		}
		if view.snapshot != cached {
			t.Error("unchanged head did not reuse snapshot pointer")
		}
		if err := view.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		counts := observed.counts()
		if counts.heads[headObjectKey] != 1 || sumCounts(counts.gets) != 0 {
			t.Errorf("operations = heads %v gets %v, want one head only", counts.heads, counts.gets)
		}

		if isDir, err := store.IsDir("/"); err != nil || !isDir {
			t.Fatalf("direct IsDir = (%v, %v)", isDir, err)
		}
		if got := observed.counts().heads[headObjectKey]; got != 2 {
			t.Errorf("head calls after direct read = %d, want 2", got)
		}
	})

	t.Run("changed root fetches only changed shard", func(t *testing.T) {
		firstPath, secondPath := pathsInDistinctShards(t)
		memory := initializedMemory(t)
		initial := []readDocumentSpec{
			newReadDocument(firstPath, "# First v1\n"),
			newReadDocument(secondPath, "# Second\n"),
		}
		commitReadDocuments(t, memory, initial)
		observed := newObservedBlobStore(memory)
		store, err := Open(context.Background(), observed, Options{WorldID: testWorldID, ShardWorkers: 3})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		before := store.snapshot.Load()
		changed := initial[0]
		changed.Bodies = append(changed.Bodies, []byte("# First v2\n"))
		changed.Metadata = append(changed.Metadata, defaultReadMetadata(firstPath))
		commit := commitReadDocuments(t, memory, []readDocumentSpec{changed, initial[1]})
		observed.reset()

		view, err := store.openReadView()
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		defer func() {
			if err := view.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		}()
		after := view.snapshot
		if after == before {
			t.Fatal("changed head reused old snapshot")
		}
		changedIndex := shardIndexForPath(t, firstPath)
		unchangedIndex := shardIndexForPath(t, secondPath)
		if &before.Shards[unchangedIndex].Entries[0] != &after.Shards[unchangedIndex].Entries[0] {
			t.Error("unchanged shard entries were not reused")
		}
		counts := observed.counts()
		changedShardKey := commit.root.Shards[changedIndex].Key
		if counts.heads[headObjectKey] != 1 || counts.gets[headObjectKey] != 1 || counts.gets[commit.rootRef.Key] != 1 || counts.gets[changedShardKey] != 1 {
			t.Errorf("refresh operations = heads %v gets %v", counts.heads, counts.gets)
		}
		if sumCounts(counts.gets) != 3 {
			t.Errorf("refresh Get count = %d, want head, root, one shard", sumCounts(counts.gets))
		}
		if counts.gets[commit.root.Shards[unchangedIndex].Key] != 0 {
			t.Error("unchanged shard was fetched")
		}
	})

	t.Run("corrupt changed head never serves cache", func(t *testing.T) {
		memory := initializedMemory(t)
		commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "# A\n")})
		store, err := Open(context.Background(), memory, Options{WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		cached := store.snapshot.Load()
		head := getObject(t, memory, headObjectKey)
		replaceObject(t, memory, headObjectKey, head.Attributes.Generation, []byte("{"))

		view, err := store.OpenReadView()
		if view != nil || !errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("OpenReadView() = (%v, %v), want integrity", view, err)
		}
		if store.snapshot.Load() != cached {
			t.Error("failed refresh replaced cached snapshot")
		}
		if _, err := store.Get("/docs/a.md", 0); !errors.Is(err, blob.ErrIntegrity) {
			t.Errorf("direct Get error = %v, want integrity instead of stale data", err)
		}
		if got, err := store.CurrentVersion("/docs/a.md"); got != 0 || !errors.Is(err, blob.ErrIntegrity) {
			t.Errorf("CurrentVersion during failed refresh = (%d, %v), want integrity", got, err)
		}
	})

	t.Run("replayed sequence never replaces cache", func(t *testing.T) {
		memory := initializedMemory(t)
		commitReadDocuments(t, memory, []readDocumentSpec{newReadDocument("/docs/a.md", "# A\n")})
		store, err := Open(context.Background(), memory, Options{WorldID: testWorldID})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		cached := store.snapshot.Load()
		head := getObject(t, memory, headObjectKey)
		replaceObject(t, memory, headObjectKey, head.Attributes.Generation, head.Data)
		view, err := store.OpenReadView()
		if view != nil || !errors.Is(err, blob.ErrIntegrity) {
			t.Fatalf("OpenReadView() = (%v, %v), want sequence integrity", view, err)
		}
		if store.snapshot.Load() != cached {
			t.Error("replayed sequence replaced cached snapshot")
		}
	})

	t.Run("serialized refresh cannot install older root", func(t *testing.T) {
		memory := initializedMemory(t)
		base := newReadDocument("/docs/a.md", "# A v1\n")
		commitReadDocuments(t, memory, []readDocumentSpec{base})
		interleaved := newInterleavingBlobStore(memory)
		store, err := Open(context.Background(), interleaved, Options{WorldID: testWorldID, RequestTimeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("open: %v", err)
		}

		second := base
		second.Bodies = append(second.Bodies, []byte("# A v2\n"))
		second.Metadata = append(second.Metadata, defaultReadMetadata(second.Path))
		secondCommit := commitReadDocuments(t, memory, []readDocumentSpec{second})
		interleaved.block(secondCommit.rootRef.Key)
		firstResult := make(chan readViewResult, 1)
		go func() {
			view, err := store.openReadView()
			firstResult <- readViewResult{view: view, err: err}
		}()
		interleaved.waitBlocked(t)

		third := second
		third.Bodies = append(third.Bodies, []byte("# A v3\n"))
		third.Metadata = append(third.Metadata, defaultReadMetadata(third.Path))
		thirdCommit := commitReadDocuments(t, memory, []readDocumentSpec{third})
		interleaved.watch(thirdCommit.headGeneration)
		secondResult := make(chan readViewResult, 1)
		go func() {
			view, err := store.openReadView()
			secondResult <- readViewResult{view: view, err: err}
		}()
		interleaved.waitWatched(t)
		interleaved.release()

		older := receiveReadView(t, firstResult)
		newer := receiveReadView(t, secondResult)
		defer closeTestReadView(t, older)
		defer closeTestReadView(t, newer)
		assertViewBody(t, older, "# A v2\n", 2)
		assertViewBody(t, newer, "# A v3\n", 3)
		if installed := store.snapshot.Load(); installed.Head.Root != thirdCommit.rootRef {
			t.Errorf("installed root = %+v, want newest %+v", installed.Head.Root, thirdCommit.rootRef)
		}
	})
}

func TestReadViewContextAndSnapshot(t *testing.T) {
	memory := initializedMemory(t)
	first := newReadDocument("/docs/a.md", "# A v1\n")
	first.Metadata[0] = readMetadata("A", "before", "context")
	commitReadDocuments(t, memory, []readDocumentSpec{first})
	observed := newObservedBlobStore(memory)
	store, err := Open(context.Background(), observed, Options{WorldID: testWorldID, RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	observed.reset()
	old, err := store.openReadView()
	if err != nil {
		t.Fatalf("open old view: %v", err)
	}
	deadline, ok := old.ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 2*time.Second {
		t.Fatalf("view deadline = %v, want active two-second bound", deadline)
	}

	second := first
	second.Bodies = append(second.Bodies, []byte("# A v2\n"))
	second.Metadata = append(second.Metadata, readMetadata("A", "after", "context"))
	commitReadDocuments(t, memory, []readDocumentSpec{second, newReadDocument("/docs/new.md", "# New\n")})
	assertPinnedReadView(t, old, &pinnedReadWant{
		body:          "# A v1\n",
		version:       1,
		versions:      []int{1},
		entries:       []protocolstore.DirEntry{{Name: "a.md"}},
		lookup:        "before",
		missingLookup: "after",
		missingBody:   "# A v2\n",
	})
	for _, used := range observed.counts().contexts {
		if used != old.ctx {
			t.Error("view operation used a different context")
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
		entries:       []protocolstore.DirEntry{{Name: "a.md"}, {Name: "new.md"}},
		lookup:        "after",
		missingLookup: "before",
		missingBody:   "# A v1\n",
	})
	closeTestReadView(t, fresh)
	if err := old.Close(); err != nil {
		t.Fatalf("close old: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("second close old: %v", err)
	}
	if !errors.Is(old.ctx.Err(), context.Canceled) {
		t.Errorf("closed context error = %v, want canceled", old.ctx.Err())
	}
	if _, err := old.IsDir("/"); !errors.Is(err, context.Canceled) {
		t.Errorf("read after close error = %v, want canceled", err)
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
	store, err := Open(context.Background(), observed, Options{WorldID: testWorldID})
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
		if document.ETag != protocolstore.StoredETag(wantRaw) || document.Metadata["tags"] != "after,common" {
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
				if _, err := view.Get(test.path, test.version); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Get() error = %v, want not-exist", err)
				}
			})
		}
		if _, err := store.Get("/missing.md", 0); err != os.ErrNotExist {
			t.Errorf("direct Get error = %v, want exact os.ErrNotExist", err)
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
		if _, err := view.Versions("/missing.md"); !errors.Is(err, os.ErrNotExist) {
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
		if err := view.VerifyChain("/missing.md"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing chain error = %v", err)
		}
	})

	t.Run("directory topology", func(t *testing.T) {
		assertEntries(t, view, "/", false, []protocolstore.DirEntry{{Name: "docs", IsDir: true}})
		assertEntries(t, view, "/", true, []protocolstore.DirEntry{{Name: "archive", IsDir: true}, {Name: "docs", IsDir: true}})
		assertEntries(t, view, "/docs", false, []protocolstore.DirEntry{
			{Name: "live.md"},
			{Name: "pruned.md"},
			{Name: "shared-a.md"},
			{Name: "shared-z.md"},
		})
		assertEntries(t, view, "/archive", false, []protocolstore.DirEntry{})
		if _, err := view.ListEntries(live.Path, false); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("list document error = %v", err)
		}
		if _, err := view.ListEntries("/missing", false); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("list missing error = %v", err)
		}
		if _, err := view.ListEntries("/../docs", false); !errors.Is(err, os.ErrNotExist) {
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
			if isDirectory != check.want || errors.Is(err, os.ErrNotExist) != check.wantErr {
				t.Errorf("IsDir(%q) = (%v, %v), want (%v, not-exist=%v)", check.path, isDirectory, err, check.want, check.wantErr)
			}
		}
	})

	t.Run("hash and catalog", func(t *testing.T) {
		sharedHash := protocolstore.ContentHash([]byte("same"))
		path, err := view.LookupHash(sharedHash)
		if err != nil || path != sharedA.Path {
			t.Errorf("LookupHash = (%q, %v), want %q", path, err, sharedA.Path)
		}
		if _, err := view.LookupHash(protocolstore.ContentHash([]byte("# Archived\n"))); !errors.Is(err, os.ErrNotExist) {
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
		version, err := store.CurrentVersion(live.Path)
		if err != nil || version != 2 {
			t.Errorf("CurrentVersion = (%d, %v)", version, err)
		}
		if got, err := store.CurrentVersion("/missing.md"); got != 0 || err != nil {
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
			name:    "archived non-tip",
			version: 1,
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				raw, err := protocolstore.SetArchived(document.versions[1].raw, true)
				if err != nil {
					t.Fatalf("archive raw: %v", err)
				}
				installRawMutation(t, memory, document, 1, raw)
			},
		},
		{
			name: "tip archive mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				raw, err := protocolstore.SetArchived(document.versions[2].raw, true)
				if err != nil {
					t.Fatalf("archive raw: %v", err)
				}
				installRawMutation(t, memory, document, 2, raw)
			},
		},
		{
			name: "tip body hash mismatch",
			mutate: func(t *testing.T, memory *blob.Memory, commit *readCommit) {
				document := commit.documents["/docs/a.md"]
				entry := document.entry
				entry.BodyHash = protocolstore.ContentHash([]byte("other"))
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
			store, err := Open(context.Background(), memory, Options{WorldID: testWorldID})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			view, err := store.openReadView()
			if err != nil {
				t.Fatalf("open view: %v", err)
			}
			defer closeTestReadView(t, view)
			_, err = view.Get("/docs/a.md", test.version)
			if !errors.Is(err, blob.ErrIntegrity) {
				t.Fatalf("Get error = %v, want integrity", err)
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Errorf("Get error = %v, want provider cause %v", err, test.wantCause)
			}
		})
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
		raw, err := protocolstore.SerializeVersion(1, nil, []byte("changed"), document.versions[1].metadata)
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
		store, err := Open(context.Background(), observed, Options{WorldID: testWorldID})
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
	store, err := Open(context.Background(), memory, Options{WorldID: testWorldID, RequestTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.objects = &blockingHeadStore{Store: memory}
	view, err := store.OpenReadView()
	if view != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("OpenReadView() = (%v, %v), want deadline", view, err)
	}
}

type readDocumentSpec struct {
	Path       string
	Bodies     [][]byte
	Metadata   []map[string]string
	Archived   bool
	RetainFrom int
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
	documents      map[string]*committedReadDocument
	root           rootObject
	rootRef        objectRef
	headGeneration blob.Generation
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
	commit.headGeneration = getObject(t, memory, headObjectKey).Attributes.Generation
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
		raw, err := protocolstore.SerializeVersion(version, previousRaw, body, spec.Metadata[index])
		if err != nil {
			t.Fatalf("serialize %s v%d: %v", spec.Path, version, err)
		}
		if spec.Archived && version == len(spec.Bodies) {
			raw, err = protocolstore.SetArchived(raw, true)
			if err != nil {
				t.Fatalf("archive %s v%d: %v", spec.Path, version, err)
			}
		}
		previousRaw = raw
		modified := readVersionModified(version)
		entry := historyEntry{
			Version:  version,
			BodyHash: protocolstore.ContentHash(body),
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
	metadata := protocolstore.ExtractMetadata(tip.raw)
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
	history.Entries[index].BodyHash = protocolstore.ContentHash(protocolstore.ExtractBody(raw))
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
	_, root := readHeadAndRoot(t, memory)
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

func openTestReadView(t *testing.T, memory blob.Store) *readView {
	t.Helper()
	store, err := Open(context.Background(), memory, Options{WorldID: testWorldID})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	view, err := store.openReadView()
	if err != nil {
		t.Fatalf("open test view: %v", err)
	}
	return view
}

func closeTestReadView(t *testing.T, view *readView) {
	t.Helper()
	if view != nil {
		if err := view.Close(); err != nil {
			t.Errorf("close read view: %v", err)
		}
	}
}

func assertViewBody(t *testing.T, view *readView, wantBody string, wantVersion int) {
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
	entries       []protocolstore.DirEntry
	lookup        string
	missingLookup string
	missingBody   string
}

func assertPinnedReadView(t *testing.T, view *readView, want *pinnedReadWant) {
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
	hash := protocolstore.ContentHash([]byte(want.body))
	if path, err := view.LookupHash(hash); err != nil || path != "/docs/a.md" {
		t.Errorf("LookupHash = (%q, %v)", path, err)
	}
	if _, err := view.LookupHash(protocolstore.ContentHash([]byte(want.missingBody))); !errors.Is(err, os.ErrNotExist) {
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

func assertEntries(t *testing.T, view *readView, requestPath string, includeArchived bool, want []protocolstore.DirEntry) {
	t.Helper()
	entries, err := view.ListEntries(requestPath, includeArchived)
	if err != nil {
		t.Fatalf("ListEntries(%q): %v", requestPath, err)
	}
	if !slices.Equal(entries, want) {
		t.Errorf("ListEntries(%q, %v) = %+v, want %+v", requestPath, includeArchived, entries, want)
	}
}

func pathsInDistinctShards(t *testing.T) (firstPath, secondPath string) {
	t.Helper()
	firstPath = "/docs/a.md"
	firstIndex := shardIndexForPath(t, firstPath)
	for index := range 10_000 {
		candidate := fmt.Sprintf("/docs/item-%04d.md", index)
		if shardIndexForPath(t, candidate) != firstIndex {
			return firstPath, candidate
		}
	}
	t.Fatal("could not find paths in distinct shards")
	return "", ""
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

type interleavingBlobStore struct {
	blob.Store
	mu              sync.Mutex
	blockKey        string
	watchGeneration blob.Generation
	watchSet        bool
	blocked         chan struct{}
	unblock         chan struct{}
	watched         chan struct{}
	blockOnce       atomic.Bool
	watchOnce       atomic.Bool
	releaseOnce     sync.Once
}

func newInterleavingBlobStore(store blob.Store) *interleavingBlobStore {
	return &interleavingBlobStore{
		Store:   store,
		blocked: make(chan struct{}),
		unblock: make(chan struct{}),
		watched: make(chan struct{}),
	}
}

func (store *interleavingBlobStore) block(key string) {
	store.mu.Lock()
	store.blockKey = key
	store.mu.Unlock()
}

func (store *interleavingBlobStore) watch(generation blob.Generation) {
	store.mu.Lock()
	store.watchGeneration = generation
	store.watchSet = true
	store.mu.Unlock()
}

func (store *interleavingBlobStore) Get(ctx context.Context, key string) (blob.Object, error) {
	store.mu.Lock()
	blockKey := store.blockKey
	store.mu.Unlock()
	if key == blockKey && store.blockOnce.CompareAndSwap(false, true) {
		close(store.blocked)
		select {
		case <-store.unblock:
		case <-ctx.Done():
			return blob.Object{}, &blob.OpError{Op: "get", Key: key, Err: ctx.Err()}
		}
	}
	return store.Store.Get(ctx, key)
}

func (store *interleavingBlobStore) Head(ctx context.Context, key string) (blob.Attributes, error) {
	attributes, err := store.Store.Head(ctx, key)
	if err != nil {
		return attributes, err
	}
	store.mu.Lock()
	watchGeneration := store.watchGeneration
	watchSet := store.watchSet
	store.mu.Unlock()
	if watchSet && attributes.Generation == watchGeneration && store.watchOnce.CompareAndSwap(false, true) {
		close(store.watched)
	}
	return attributes, nil
}

func (store *interleavingBlobStore) waitBlocked(t *testing.T) {
	t.Helper()
	waitForTestSignal(t, store.blocked, "blocked root read")
}

func (store *interleavingBlobStore) waitWatched(t *testing.T) {
	t.Helper()
	waitForTestSignal(t, store.watched, "new head observation")
}

func (store *interleavingBlobStore) release() {
	store.releaseOnce.Do(func() { close(store.unblock) })
}

type readViewResult struct {
	view *readView
	err  error
}

func receiveReadView(t *testing.T, results <-chan readViewResult) *readView {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("open concurrent read view: %v", result.err)
		}
		return result.view
	case <-timer.C:
		t.Fatal("timed out waiting for concurrent read view")
		return nil
	}
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

type blockingHeadStore struct {
	blob.Store
}

func (store *blockingHeadStore) Head(ctx context.Context, key string) (blob.Attributes, error) {
	<-ctx.Done()
	return blob.Attributes{}, &blob.OpError{Op: "head", Key: key, Err: ctx.Err()}
}
