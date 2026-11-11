package graphstore

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/memory/graph"
	"github.com/latebit-io/demarkus/protocol/memtest"
)

// shownState is everything a reader can see of the selected graph.
type shownState struct {
	nodes     map[string]StoredNode
	edges     []StoredEdge
	incoming  map[string][]StoredEdge
	revisions map[string]int
}

func shownOf(s *Store) shownState {
	state := shownState{nodes: map[string]StoredNode{}, incoming: map[string][]StoredEdge{}, revisions: maps.Clone(s.sourceRevisions)}
	for key, node := range s.nodes {
		state.nodes[key] = *node
	}
	state.edges = s.liveEdgesLocked()
	slices.SortFunc(state.edges, compareSnapshotEdges)
	for to, indices := range s.incoming {
		for _, index := range indices {
			state.incoming[to] = append(state.incoming[to], s.edges[index])
		}
		slices.SortFunc(state.incoming[to], compareSnapshotEdges)
	}
	return state
}

func (a *shownState) equal(b *shownState) bool {
	return maps.Equal(a.nodes, b.nodes) && slices.Equal(a.edges, b.edges) && maps.Equal(a.revisions, b.revisions) &&
		maps.EqualFunc(a.incoming, b.incoming, slices.Equal[[]StoredEdge])
}

func observeDoc(store *Store, i, version, links int) {
	body := fmt.Sprintf("# Doc %d v%d\n", i, version)
	for j := range links {
		body += fmt.Sprintf("\n[d%d](/doc-%d.md)", j, (i+j+1)%40)
	}
	store.ObserveDocument(fmt.Sprintf("mark://w/doc-%d.md", i), graph.FetchResult{Status: "ok", Body: body,
		Metadata: map[string]string{"version": fmt.Sprint(version), "etag": fmt.Sprintf("e%d-%d", i, version)}})
}

// Merge reselects only the sources it was given; what it leaves must be
// exactly what reselecting every source would show.
func TestMergePatchesWhatAFullRebuildWouldShow(t *testing.T) {
	store := New()
	check := func(step string) {
		t.Helper()
		store.mu.Lock()
		defer store.mu.Unlock()
		patched := shownOf(store)
		store.rebuildSeedsLocked()
		if rebuilt := shownOf(store); !patched.equal(&rebuilt) {
			t.Fatalf("%s: the patched store differs from a full rebuild\npatched %+v\nrebuilt %+v", step, patched, rebuilt)
		}
	}
	for i := range 40 {
		observeDoc(store, i, 1, i%5)
		check(fmt.Sprintf("observe %d", i))
	}
	seedRevision(store, "hub", revisionGraph(3, "target"))
	check("seed")
	for i := range 40 {
		switch i % 4 {
		case 0:
			observeDoc(store, i, 2, (i+3)%6) // a newer revision with other links
		case 1:
			store.ObserveDocument(fmt.Sprintf("mark://w/doc-%d.md", i), graph.FetchResult{Status: "not-found"})
		case 2:
			observeDoc(store, i, 1, 9) // the same revision claiming other links: a conflict
		case 3:
			store.ObserveDocument(fmt.Sprintf("mark://w/doc-%d.md", i), graph.FetchResult{Status: "server-error"})
		}
		check(fmt.Sprintf("change %d", i))
	}
	store.Merge(revisionGraph(4, "moved"), nil)
	check("local over the seed")
	store.ObserveDocument("mark://w/unlinked.md", graph.FetchResult{Status: "ok", Body: "# U\n\n[d](/doc-1.md)", Metadata: map[string]string{"version": "1", "etag": "u1"}})
	store.ObserveDocument("mark://w/unlinked.md", graph.FetchResult{Status: "not-found"})
	check("a document read, then gone")
	if dropped := store.Sweep(time.Now().Add(2 * deadRetention)); dropped != 1 {
		t.Fatalf("swept %d sources, want the dead one nothing links to", dropped)
	}
	check("sweep")
}

// One observed document costs its own edges: every other source keeps the
// node and the edge slots it had.
func TestMergeLeavesOtherSourcesInPlace(t *testing.T) {
	store := New()
	for i := range 40 {
		observeDoc(store, i, 1, 3)
	}
	other, edges := store.nodes["mark://w/doc-7.md"], &store.edges[0]
	observeDoc(store, 3, 2, 5)
	if store.nodes["mark://w/doc-7.md"] != other || &store.edges[0] != edges {
		t.Fatal("observing one document rebuilt the store")
	}
	if got := store.GetNode("mark://w/doc-3.md"); got.Observation.Revision != 2 || store.EdgeCount() != 39*3+5 {
		t.Fatalf("doc-3 = %+v with %d edges, want revision 2 and %d", got, store.EdgeCount(), 39*3+5)
	}
}

// A dead source is a marker, kept while it is needed: a seed claims the
// document, something links to it, or it died too recently to rule out a
// stale seed. Past that it is forgotten whole.
func TestSweepDropsDeadSourcesNothingNeeds(t *testing.T) {
	store := New()
	const lonely, linked, owned = "mark://w/lonely.md", "mark://w/linked.md", freshnessSource
	store.ObserveDocument(lonely, graph.FetchResult{Status: "ok", Body: "# L\n\n[x](/x.md)", Metadata: map[string]string{"version": "1", "etag": "l1"}})
	store.ObserveDocument(lonely, graph.FetchResult{Status: "archived", Metadata: map[string]string{"version": "2", "etag": "l2"}})
	store.ObserveDocument(linked, graph.FetchResult{Status: "not-found"})
	store.ObserveDocument("mark://w/hub.md", graph.FetchResult{Status: "ok", Body: "# Hub\n\n[l](/linked.md)", Metadata: map[string]string{"version": "1", "etag": "h1"}})
	seedRevision(store, "hub", revisionGraph(3, "target"))
	dead := revisionGraph(3, "")
	dead.GetNode(owned).Status = "archived"
	store.Merge(dead, nil)

	if store.Sweep(time.Now().Add(deadRetention/2)) != 0 || store.GetNode(lonely) == nil {
		t.Fatal("a source dead for less than the retention was dropped")
	}
	if dropped := store.Sweep(time.Now().Add(2 * deadRetention)); dropped != 1 {
		t.Fatalf("swept %d sources, want the one nothing needs", dropped)
	}
	if store.GetNode(lonely) != nil || store.GetNode(linked) == nil || store.GetNode(owned) == nil {
		t.Fatalf("after the sweep: lonely %v linked %v owned %v", store.GetNode(lonely), store.GetNode(linked), store.GetNode(owned))
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, held := store.localSources[lonely]; held || store.sourceRevisions[lonely] != 0 || store.sourceRefs[lonely] != 0 {
		t.Error("the swept source left a record behind")
	}
}

// A validation claim cools a source down for the freshness window; past it
// the claim is forgotten, not kept for every source ever revalidated.
func TestValidationClaimsExpire(t *testing.T) {
	store := New()
	if !store.claimValidation("mark://w/a.md") || store.claimValidation("mark://w/a.md") {
		t.Fatal("a claim must hold once inside the window")
	}
	store.mu.Lock()
	store.validating["mark://w/old.md"] = time.Now().Add(-2 * graph.FreshnessWindow)
	store.mu.Unlock()
	if !store.claimValidation("mark://w/b.md") {
		t.Fatal("a second source could not be claimed")
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, held := store.validating["mark://w/old.md"]; held || len(store.validating) != 2 {
		t.Errorf("validating holds %v, want the two live claims", slices.Collect(maps.Keys(store.validating)))
	}
}

// observedOwned is the heap a store holds after cycles in which agents read
// new inbox documents, see earlier ones archived, and a week passes.
func observedOwned(t *testing.T, cycles int) int64 {
	const live, churn = 48, 12
	return memtest.Owned(func() any {
		store := New()
		read := func(i, version int) {
			body := fmt.Sprintf("# Run 2026-09-29T15:58:33.%09dZ\n", i)
			for j := range 8 {
				body += fmt.Sprintf("\n[step %d](/agents/session-%d/inbox/01HZX%08d.md)", j, j%7, i+j+1)
			}
			store.ObserveDocument(fmt.Sprintf("mark://w/agents/session-%d/inbox/01HZX%08d.md", i%7, i), graph.FetchResult{Status: "ok", Body: body,
				Metadata: map[string]string{"version": fmt.Sprint(version), "etag": fmt.Sprintf("sha256-%064x", i*31+version)}})
		}
		for i := range live {
			read(i, 1)
		}
		for cycle := range cycles {
			first := cycle * churn
			for i := first; i < first+churn; i++ {
				store.ObserveDocument(fmt.Sprintf("mark://w/agents/session-%d/inbox/01HZX%08d.md", i%7, i), graph.FetchResult{Status: "archived"})
				read(i+live, 1)
			}
			read(first+churn+1, 2) // an edit of a live document
			store.Sweep(time.Now().Add(time.Duration(cycle+2) * deadRetention))
		}
		if nodes := store.NodeCount(); nodes < live || nodes > live+2*churn+8 {
			t.Fatalf("%d nodes shown, want about the %d live documents", nodes, live)
		}
		return store
	})
}

// What the store keeps tracks the documents that are live or still linked,
// not every document it ever read.
func TestObservedStoreRetainsOnlyLiveSources(t *testing.T) {
	short, long := observedOwned(t, 20), observedOwned(t, 120)
	t.Logf("the store holds %d bytes after 20 cycles, %d after 120", short, long)
	// 1200 more archived documents remembered would be several hundred KB.
	if grew := long - short; grew > 96<<10 {
		t.Errorf("the store grew %d bytes over 100 more cycles of churn", grew)
	}
}
