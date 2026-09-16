package graphstore

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
)

func TestRevalidateSavesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	nodes := make([]StoredNode, 0, 3)
	for i := range 3 {
		nodes = append(nodes, StoredNode{URL: fmt.Sprintf("mark://source/%d.md", i), Status: "ok"})
	}
	store.ReplaceSeed("owner", nodes, nil)
	persistedRevisions := func() []int {
		t.Helper()
		reloaded, err := Load(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		revisions := make([]int, 0, 3)
		for i := range 3 {
			if node := reloaded.GetNode(fmt.Sprintf("mark://source/%d.md", i)); node != nil {
				revisions = append(revisions, node.Observation.Revision)
			}
		}
		return revisions
	}
	fetches := 0
	fetchFn := func(_ context.Context, _, path string) (graph.FetchResult, error) {
		fetches++
		if fetches == 3 && slices.Contains(persistedRevisions(), 2) {
			t.Fatal("an earlier attempt was persisted before the pass finished")
		}
		return graph.FetchResult{Status: "ok", Body: "# " + path, Metadata: map[string]string{"version": "2"}}, nil
	}
	result, err := store.Revalidate(t.Context(), nil, fetchFn, fetch.ParseMarkURL)
	if err != nil || result.Attempts != 3 || result.Fetches != 3 {
		t.Fatalf("revalidation: %+v, %v", result, err)
	}
	if got := persistedRevisions(); !slices.Equal(got, []int{2, 2, 2}) {
		t.Fatalf("persisted revisions = %v, want all 2 after one save", got)
	}
}

func TestFreshnessSummaryForCountsOnlyGivenSources(t *testing.T) {
	store := New()
	fresh := revisionGraph(3, "")
	store.Merge(fresh, nil)
	stale := graph.New()
	observation := graph.Observe("mark://source/stale.md", map[string]string{"version": "1"})
	observation.Complete = true
	observation.ObservedAt = time.Now().Add(-time.Hour)
	stale.AddNode(&graph.Node{URL: "mark://source/stale.md", Status: "ok", Observation: observation})
	store.Merge(stale, nil)
	// Noise the summary must ignore.
	store.Merge(revisionGraphFor("mark://source/other.md", 5, ""), nil)

	got := store.FreshnessSummaryFor([]string{freshnessSource, "mark://source/stale.md", "mark://source/missing.md"})
	if want := "freshness: 1 fresh, 1 stale, 1 unknown"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestSeedRecordCacheFollowsOwnerWrites(t *testing.T) {
	store := New()
	seedRevision(store, "owner", revisionGraph(3, "a"))
	if node := store.GetNode(freshnessSource); node == nil || node.Observation.Problem != "" {
		t.Fatalf("seeded node = %+v", node)
	}
	store.MarkSeedFailure("owner")
	if node := store.GetNode(freshnessSource); node == nil || node.Observation.Problem != "seed-refresh-failed" {
		t.Fatalf("after failure = %+v", node)
	}
	seedRevision(store, "owner", revisionGraph(4, "b"))
	node := store.GetNode(freshnessSource)
	if node == nil || node.Observation.Problem != "" || node.Observation.Revision != 4 {
		t.Fatalf("after replace = %+v", node)
	}
	requireTargets(t, store, "b")

	failed := graph.New()
	failed.AddNode(&graph.Node{URL: freshnessSource, Status: "error", Observation: graph.Observation{Source: freshnessSource, AttemptedAt: time.Now()}})
	store.Merge(failed, nil)
	if node := store.GetNode(freshnessSource); node == nil || node.Observation.Problem != "incomplete" {
		t.Fatalf("after failed read = %+v", node)
	}
	requireTargets(t, store, "b")
}

func TestRevalidateCanonicalizesRequestedSources(t *testing.T) {
	store := New()
	store.ReplaceSeed("owner", []StoredNode{{URL: "mark://host/a.md", Status: "ok"}}, nil)
	fetchFn := func(_ context.Context, _, _ string) (graph.FetchResult, error) {
		return graph.FetchResult{Status: "ok", Body: "# A", Metadata: map[string]string{"version": "2"}}, nil
	}
	result, err := store.Revalidate(t.Context(), []string{"mark://host:6309/a.md"}, fetchFn, fetch.ParseMarkURL)
	if err != nil || result.Attempts != 1 || result.Fetches != 1 {
		t.Fatalf("dial-address source skipped: %+v, %v", result, err)
	}
	if node := store.GetNode("mark://host/a.md"); node == nil || node.Observation.Revision != 2 {
		t.Fatalf("source not refreshed: %+v", node)
	}
}
