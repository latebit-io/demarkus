package graphstore

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/graph"
)

func seedObservedDocument(store *Store, owner, docURL, body string, metadata map[string]string) {
	seed := New()
	seed.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: body, Metadata: metadata})
	nodes, edges := seed.Snapshot()
	store.ReplaceSeed(owner, nodes, edges)
}

func TestObserveDocumentReplacesMetadataRelations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	store, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	docURL := "mark://world/source.md"
	body := "# Source\n\n[body](/body.md)\n"
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: body,
		Metadata: map[string]string{"version": "1", "etag": "one", "rel-depends-on": "/old.md"},
	})
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: body,
		Metadata: map[string]string{"version": "2", "etag": "two", "rel-related": "/new.md"},
	})

	page, err := store.Neighborhood(docURL, NeighborhoodOptions{Direction: NeighborhoodOutgoing})
	if err != nil {
		t.Fatal(err)
	}
	if got := neighborhoodURLs(page); !slices.Equal(got, []string{"mark://world/body.md", "mark://world/new.md"}) {
		t.Fatalf("outgoing URLs = %v", got)
	}
	if page.Rows[0].Edges[0].Edge.Rel != "" || page.Rows[1].Edges[0].Edge.Rel != "related" {
		t.Fatalf("outgoing evidence = %+v", page.Rows)
	}
	for i := range page.Rows {
		row := &page.Rows[i]
		if source := row.Edges[0].Source; source.URL != docURL || source.Title != "Source" || source.LinkCount != 1 || source.Observation.Revision != 2 || source.Etag != "two" {
			t.Fatalf("source evidence = %+v", source)
		}
	}
	if node := store.GetNode(docURL); node == nil || node.Title != "Source" || node.LinkCount != 1 || node.Observation.Revision != 2 {
		t.Fatalf("observed node = %+v", node)
	}
	if got := store.Backlinks("mark://world/old.md"); len(got) != 0 {
		t.Fatalf("removed relation remains in legacy backlinks: %v", got)
	}
	if got := store.Backlinks("mark://world/new.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("legacy backlinks = %v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ObserveDocument persisted internally: %v", err)
	}
}

func TestObserveDocumentIncompleteKeepsLastGoodAdjacency(t *testing.T) {
	store := New()
	docURL := "mark://world/source.md"
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: "# Source\n\n[target](/target.md)",
		Metadata: map[string]string{"version": "1", "etag": "one"},
	})
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "error", Metadata: map[string]string{"version": "1", "etag": "one"},
	})
	if got := store.Backlinks("mark://world/target.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("incomplete observation replaced last-good adjacency: %v", got)
	}
	if node := store.GetNode(docURL); node == nil || node.Observation.Revision != 1 || node.Etag != "one" || node.Observation.Problem != "incomplete" {
		t.Fatalf("incomplete observation replaced node: %+v", node)
	}
}

func TestObserveDocumentEqualRepresentationRefreshesWithoutRebuild(t *testing.T) {
	store := New()
	docURL := "mark://world/source.md"
	metadata := map[string]string{"version": "3", "etag": "stable", "rel-depends-on": "/dependency.md"}
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: "# Source\n\n[old](/old.md)", Metadata: metadata,
	})

	stale := time.Now().Add(-graph.FreshnessWindow - time.Minute)
	store.mu.Lock()
	local := store.localSources[docURL]
	local.Node.Observation.ObservedAt = stale
	local.Node.Observation.AttemptedAt = stale
	local.Node.CrawledAt = stale
	store.localSources[docURL] = local
	store.nodes[docURL].Observation = local.Node.Observation
	store.nodes[docURL].CrawledAt = stale
	edgeAddress := &store.edges[0]
	store.mu.Unlock()

	if !store.ObserveDocument(docURL, graph.FetchResult{
		Source: "mark://world:6309/source.md", Status: "ok",
		Body:     "# Source\n\n[old](/old.md)",
		Metadata: metadata,
	}) {
		t.Fatal("equal observation was rejected")
	}
	node := store.GetNode(docURL)
	if node.Title != "Source" || node.LinkCount != 1 || node.Observation.Freshness() != "fresh" || !node.Observation.ObservedAt.After(stale) {
		t.Fatalf("refreshed node = %+v", node)
	}
	if got := store.Backlinks("mark://world/old.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("old body edge changed: %v", got)
	}
	if got := store.Backlinks("mark://world/dependency.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("old relation changed: %v", got)
	}
	if got := store.Backlinks("mark://world/new.md"); len(got) != 0 {
		t.Fatalf("equal representation was reparsed: %v", got)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if &store.edges[0] != edgeAddress {
		t.Fatal("equal representation rebuilt topology")
	}
	if !store.localSources[docURL].Node.Observation.ObservedAt.Equal(node.Observation.ObservedAt) {
		t.Fatal("local evidence timestamp was not refreshed")
	}
}

func TestObserveDocumentEqualIdentityChangedRepresentationFallsBack(t *testing.T) {
	store := New()
	docURL := "mark://world/source.md"
	metadata := map[string]string{"version": "3", "etag": "stable", "rel-depends-on": "/dependency.md"}
	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: "# Source\n\n[old](/old.md)", Metadata: metadata,
	})

	store.ObserveDocument(docURL, graph.FetchResult{
		Status: "ok", Body: "# Changed\n\n[new](/new.md)",
		Metadata: map[string]string{"version": "3", "etag": "stable", "rel-depends-on": "/changed.md"},
	})

	if node := store.GetNode(docURL); node.Title != "Source" || node.Observation.Problem != "revision-conflict" {
		t.Fatalf("changed representation bypassed reconciliation: %+v", node)
	}
	if got := store.Backlinks("mark://world/old.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("last-good body edge changed: %v", got)
	}
	if got := store.Backlinks("mark://world/dependency.md"); !slices.Equal(got, []string{docURL}) {
		t.Fatalf("last-good relation changed: %v", got)
	}
}

func TestRememberDocumentRepresentationRejectsUnacceptedContent(t *testing.T) {
	store := New()
	docURL := "mark://world/source.md"
	metadata := map[string]string{"version": "3", "etag": "stable"}
	body := "# Source\n\n[old](/old.md)"
	store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: body, Metadata: metadata})

	node := &graph.Node{URL: docURL, Title: "Changed", Status: "ok", LinkCount: 1, Observation: graph.Observe(docURL, metadata)}
	node.Observation.Complete = true
	changedBody := "# Changed\n\n[new](/new.md)"
	store.rememberDocumentRepresentation(
		node,
		graph.ExtractDocumentEdges(docURL, changedBody, metadata),
		hashDocumentRepresentation(changedBody, metadata),
	)

	if _, exists := store.representations[docURL]; exists {
		t.Fatal("unaccepted representation was cached")
	}
	store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: changedBody, Metadata: metadata})
	if stored := store.GetNode(docURL); stored.Title != "Source" || stored.Observation.Problem != "revision-conflict" {
		t.Fatalf("unaccepted representation bypassed reconciliation: %+v", stored)
	}
}

func TestObserveDocumentMissingIdentityFallsBack(t *testing.T) {
	t.Run("revision", func(t *testing.T) {
		store := New()
		docURL := "mark://world/source.md"
		metadata := map[string]string{"etag": "stable"}
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: "# Old\n\n[old](/old.md)", Metadata: metadata})
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: "# New\n\n[new](/new.md)", Metadata: metadata})
		if node := store.GetNode(docURL); node.Title != "New" {
			t.Fatalf("revisionless observation did not use full merge: %+v", node)
		}
	})

	t.Run("etag", func(t *testing.T) {
		store := New()
		docURL := "mark://world/source.md"
		metadata := map[string]string{"version": "1"}
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: "# Old\n\n[old](/old.md)", Metadata: metadata})
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: "# New\n\n[new](/new.md)", Metadata: metadata})
		if node := store.GetNode(docURL); node.Title != "Old" || node.Observation.Problem != "revision-conflict" {
			t.Fatalf("etagless conflict semantics changed: %+v", node)
		}
	})
}

func TestObserveDocumentChangedIdentityFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		result      graph.FetchResult
		wantTitle   string
		wantProblem string
	}{
		{name: "revision", result: graph.FetchResult{Status: "ok", Body: "# New\n\n[new](/new.md)", Metadata: map[string]string{"version": "2", "etag": "two"}}, wantTitle: "New"},
		{name: "etag", result: graph.FetchResult{Status: "ok", Body: "# New\n\n[new](/new.md)", Metadata: map[string]string{"version": "1", "etag": "two"}}, wantTitle: "Old", wantProblem: "revision-conflict"},
		{name: "source", result: graph.FetchResult{Source: "mark://other/source.md", Status: "ok", Body: "# New\n\n[new](/new.md)", Metadata: map[string]string{"version": "1", "etag": "one"}}, wantTitle: "Old", wantProblem: "revision-conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := New()
			docURL := "mark://world/source.md"
			store.ObserveDocument(docURL, graph.FetchResult{
				Status: "ok", Body: "# Old\n\n[old](/old.md)", Metadata: map[string]string{"version": "1", "etag": "one"},
			})
			store.ObserveDocument(docURL, tc.result)
			if node := store.GetNode(docURL); node.Title != tc.wantTitle || node.Observation.Problem != tc.wantProblem {
				t.Fatalf("changed identity result = %+v", node)
			}
		})
	}
}

func TestObserveDocumentConflictAndHighWaterFallBack(t *testing.T) {
	t.Run("conflict", func(t *testing.T) {
		store := New()
		docURL := "mark://world/source.md"
		metadata := map[string]string{"version": "3", "etag": "three"}
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: "# Local\n\n[old](/old.md)", Metadata: metadata})
		seedObservedDocument(store, "hub", docURL, "# Seed\n\n[seed](/seed.md)", metadata)
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: "# Changed\n\n[new](/new.md)", Metadata: metadata})
		if local := store.localSources[docURL]; local.Node.Observation.Problem != "revision-conflict" {
			t.Fatalf("conflicting local evidence bypassed reconciliation: %+v", local.Node)
		}
	})

	t.Run("high water", func(t *testing.T) {
		store := New()
		docURL := "mark://world/source.md"
		metadata := map[string]string{"version": "3", "etag": "three"}
		body := "# Local\n\n[target](/target.md)"
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: body, Metadata: metadata})
		seedObservedDocument(store, "hub", docURL, "# Newer\n\n[newer](/newer.md)", map[string]string{"version": "7", "etag": "seven"})
		store.ReplaceSeed("hub", nil, nil)
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: body, Metadata: metadata})
		node := store.GetNode(docURL)
		if node.Observation.HighestRevision != 7 || node.Observation.Problem != "revision-regression" || node.Observation.Freshness() != "stale" {
			t.Fatalf("high-water mark was lost: %+v", node)
		}
	})
}

func TestObserveDocumentPartialAndSeedSelectedFallBack(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		store := New()
		docURL := "mark://world/source.md"
		observation := graph.Observe(docURL, map[string]string{"version": "2", "etag": "two"})
		observation.Complete = true
		partial := graph.New()
		partial.AddNode(&graph.Node{URL: docURL, Status: "ok", Title: "Partial", Incomplete: true, Observation: observation})
		partial.AddEdge(docURL, "mark://world/partial.md")
		store.Merge(partial, nil)
		store.ObserveDocument(docURL, graph.FetchResult{
			Status: "ok", Body: "# Complete\n\n[target](/target.md)", Metadata: map[string]string{"version": "2", "etag": "two"},
		})
		if node := store.GetNode(docURL); node.Status != "ok" || node.Title != "Complete" || !node.Observation.Complete {
			t.Fatalf("partial source did not use full merge: %+v", node)
		}
	})

	t.Run("seed selected", func(t *testing.T) {
		store := New()
		docURL := "mark://world/source.md"
		metadata := map[string]string{"version": "2", "etag": "two"}
		body := "# Seed\n\n[target](/target.md)"
		seedObservedDocument(store, "hub", docURL, body, metadata)
		store.ObserveDocument(docURL, graph.FetchResult{Status: "ok", Body: body, Metadata: metadata})
		if node := store.GetNode(docURL); node.Seeded {
			t.Fatalf("seed-selected observation bypassed local merge: %+v", node)
		}
		if _, exists := store.localSources[docURL]; !exists {
			t.Fatal("seed-selected observation did not create local evidence")
		}
	})
}

func TestObserveDocumentSkipsGeneratedGraphArtifacts(t *testing.T) {
	for _, docURL := range []string{
		"mark://world/graph.md",
		"mark://world/graph.md/v3",
		"mark://world/graph/manifest.md",
		"mark://world/graph/manifest.md/v2",
		"mark://world/graph/shards/a/edges-000.md",
		"mark://world/graph/shards/a/edges-000.md/v1",
	} {
		t.Run(docURL, func(t *testing.T) {
			store := New()
			changed := store.ObserveDocument(docURL, graph.FetchResult{
				Status: "ok", Body: "# Generated\n\n[synthetic](/target.md)",
				Metadata: map[string]string{"version": "1"},
			})
			if changed || store.NodeCount() != 0 || store.EdgeCount() != 0 {
				t.Fatalf("generated artifact entered graph: changed=%t nodes=%d edges=%d", changed, store.NodeCount(), store.EdgeCount())
			}
		})
	}
}

func TestObserveDocumentConfirmedAbsenceClearsAdjacency(t *testing.T) {
	for _, status := range []string{"not-found", "archived"} {
		t.Run(status, func(t *testing.T) {
			store := New()
			docURL := "mark://world/source.md"
			store.ObserveDocument(docURL, graph.FetchResult{
				Status: "ok", Body: "# Source\n\n[target](/target.md)",
				Metadata: map[string]string{"version": "1", "etag": "one"},
			})
			store.ObserveDocument(docURL, graph.FetchResult{
				Status: status, Metadata: map[string]string{"version": "1", "etag": "one"},
			})
			if got := store.Backlinks("mark://world/target.md"); len(got) != 0 {
				t.Fatalf("confirmed absence retained adjacency: %v", got)
			}
			if node := store.GetNode(docURL); node == nil || node.Status != status {
				t.Fatalf("confirmed absence node = %+v", node)
			}
		})
	}
}
