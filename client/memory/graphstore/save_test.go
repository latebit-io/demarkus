package graphstore

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/graph"
)

func loadAt(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func observeAt(store *Store, url string, version int, body string) {
	store.ObserveDocument(url, graph.FetchResult{Status: "ok", Body: body,
		Metadata: map[string]string{"version": strconv.Itoa(version), "etag": url + "-" + strconv.Itoa(version)}})
}

func shownURLs(store *Store) []string {
	nodes, _ := store.Snapshot()
	urls := make([]string, len(nodes))
	for i := range nodes {
		urls[i] = nodes[i].URL
	}
	return urls
}

// Two stores on one file are two processes. A save takes in what the other
// saved since, so neither drops the other's reads, and the newer read of a
// source both hold is the one kept.
func TestSaveMergesWhatAnotherProcessSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	first, second := loadAt(t, path), loadAt(t, path)
	observeAt(first, "mark://w/a.md", 1, "# A\n\n[x](/x.md)")
	observeAt(first, "mark://w/both.md", 2, "# Both v2\n\n[new](/new.md)")
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	observeAt(second, "mark://w/b.md", 1, "# B\n\n[y](/y.md)")
	observeAt(second, "mark://w/both.md", 1, "# Both v1\n\n[old](/old.md)")
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}
	want := []string{"mark://w/a.md", "mark://w/b.md", "mark://w/both.md"}
	for name, store := range map[string]*Store{"the second writer": second, "the file": loadAt(t, path)} {
		if got := shownURLs(store); !slices.Equal(got, want) {
			t.Errorf("%s holds %v, want %v", name, got, want)
		}
		if got := store.Backlinks("mark://w/new.md"); len(got) != 1 || len(store.Backlinks("mark://w/old.md")) != 0 {
			t.Errorf("%s kept the older read of both.md: new %v, old %v", name, got, store.Backlinks("mark://w/old.md"))
		}
	}
	// Nothing new on disk: the first writer's next save reads nothing back.
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	if got := shownURLs(first); !slices.Equal(got, want) {
		t.Errorf("the first writer holds %v after its second save, want %v", got, want)
	}
}

// A seed belongs to whoever refreshed it last: a process that did not
// replace an owner's seed takes the one on disk, one that did keeps its own.
func TestSaveTakesASeedAnotherProcessRefreshed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	first, second := loadAt(t, path), loadAt(t, path)
	seedRevision(first, "hub", revisionGraph(3, "three"))
	first.SetSeedEtag("hub", "generation-3")
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}
	requireTargets(t, second, "three")
	if second.SeedEtag("hub") != "generation-3" {
		t.Errorf("etag = %q, want the generation taken", second.SeedEtag("hub"))
	}
	seedRevision(second, "hub", revisionGraph(4, "four"))
	second.SetSeedEtag("hub", "generation-4")
	seedRevision(first, "hub", revisionGraph(3, "three"))
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}
	requireTargets(t, second, "four")
	requireTargets(t, loadAt(t, path), "four")
}

// A file a newer binary wrote is not this store's to overwrite.
func TestSaveRefusesAFileItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	store := loadAt(t, path)
	observeAt(store, "mark://w/a.md", 1, "# A")
	newer := []byte(`{"version": 99, "nodes": [], "edges": []}`)
	if err := os.WriteFile(path, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err == nil {
		t.Fatal("a save overwrote a newer schema")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, newer) {
		t.Errorf("file = %q, %v; want it untouched", got, err)
	}
}

// A seed replaced after a save took its snapshot is not in the file, so the
// save must not mark it clean: the next one would trade it for an older seed.
func TestSavedKeepsASeedReplacedSinceTheSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	store := loadAt(t, path)
	seedRevision(store, "hub", revisionGraph(3, "three"))
	seedRevision(store, "other", revisionGraph(1, "one"))
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	seedRevision(store, "other", revisionGraph(2, "two"))
	_, through := store.saveDocument()
	seedRevision(store, "hub", revisionGraph(4, "four"))
	if err := store.saved(through); err != nil {
		t.Fatal(err)
	}
	if store.seedDirty["hub"] == 0 {
		t.Error("a seed replaced after the snapshot was marked saved")
	}
	if store.seedDirty["other"] != 0 {
		t.Error("a seed the snapshot held stayed dirty")
	}
}
