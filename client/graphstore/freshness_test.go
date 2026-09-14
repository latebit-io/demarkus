package graphstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/protocol"
)

const freshnessSource = "mark://source/a.md"

func revisionGraph(revision int, target string) *graph.Graph {
	g := graph.New()
	observation := graph.Observe(freshnessSource, map[string]string{"version": fmt.Sprint(revision), "etag": fmt.Sprintf("etag-%d", revision)})
	observation.Complete = true
	g.AddNode(&graph.Node{URL: freshnessSource, Status: "ok", Title: "source", Observation: observation})
	if target != "" {
		g.AddEdge(freshnessSource, "mark://source/"+target)
	}
	return g
}

func seedRevision(s *Store, owner string, g *graph.Graph) {
	tmp := New()
	tmp.Merge(g, nil)
	nodes, edges := tmp.Snapshot()
	s.ReplaceSeed(owner, nodes, edges)
}

func requireTargets(t *testing.T, store *Store, targets ...string) {
	t.Helper()
	_, edges := store.Snapshot()
	got := make([]string, len(edges))
	for i, edge := range edges {
		got[i] = strings.TrimPrefix(edge.To, "mark://source/")
	}
	if len(got) != len(targets) || (len(got) > 0 && !reflect.DeepEqual(got, targets)) {
		t.Fatalf("targets = %v, want %v", got, targets)
	}
}

func TestSourceRevisionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		local, seed int
		want        string
	}{
		{"newer seed", 2, 3, "seed"}, {"older seed", 3, 2, "local"}, {"equal conflicting seed", 3, 3, "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := New()
			store.Merge(revisionGraph(tc.local, "local"), nil)
			seedRevision(store, "hub", revisionGraph(tc.seed, "seed"))
			requireTargets(t, store, tc.want)
			if tc.local == tc.seed && store.GetNode(freshnessSource).Observation.Freshness() != "stale" {
				t.Fatal("equal conflicting adjacency was not disclosed")
			}
		})
	}
	t.Run("old local crawl cannot regress seed", func(t *testing.T) {
		store := New()
		seedRevision(store, "hub", revisionGraph(8, "seed"))
		store.Merge(revisionGraph(7, "local"), nil)
		requireTargets(t, store, "seed")
	})
	t.Run("same owner cannot regress", func(t *testing.T) {
		store := New()
		seedRevision(store, "hub", revisionGraph(8, "new"))
		seedRevision(store, "hub", revisionGraph(7, "old"))
		requireTargets(t, store, "new")
	})
	t.Run("equal revision different etag", func(t *testing.T) {
		store := New()
		store.Merge(revisionGraph(3, "local"), nil)
		conflict := revisionGraph(3, "local")
		conflict.GetNode(freshnessSource).Observation.Etag = "different-version-bytes"
		seedRevision(store, "hub", conflict)
		requireTargets(t, store, "local")
		if store.GetNode(freshnessSource).Observation.Freshness() != "stale" {
			t.Fatal("equal revision etag conflict hidden")
		}
	})
	t.Run("different source revisions never compare", func(t *testing.T) {
		store := New()
		store.Merge(revisionGraph(2, "local"), nil)
		foreign := revisionGraph(99, "foreign")
		foreign.GetNode(freshnessSource).Observation.Source = "mark://other/a.md"
		seedRevision(store, "hub", foreign)
		requireTargets(t, store, "local")
		if store.GetNode(freshnessSource).Observation.Freshness() != "stale" {
			t.Fatal("incomparable source hidden")
		}
	})
}

func TestSourceRevisionEmptyDeletionAndArchive(t *testing.T) {
	for _, status := range []string{"ok", "not-found", "archived"} {
		t.Run(status, func(t *testing.T) {
			store := New()
			store.Merge(revisionGraph(3, "obsolete"), nil)
			g := revisionGraph(4, "")
			g.GetNode(freshnessSource).Status = status
			if status != "ok" {
				g.GetNode(freshnessSource).Observation.Revision = 0
				g.GetNode(freshnessSource).Observation.Etag = ""
			}
			store.Merge(g, nil)
			requireTargets(t, store)
			seedRevision(store, "old-hub", revisionGraph(3, "obsolete"))
			requireTargets(t, store)
			store.Merge(revisionGraph(5, "restored"), nil)
			requireTargets(t, store, "restored")
		})
	}
}

func TestSourceRevisionOverlappingOwnersRoundTrip(t *testing.T) {
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
		store, err := Load(filepath.Join(t.TempDir(), "graph.json"))
		if err != nil {
			t.Fatal(err)
		}
		store.Merge(revisionGraph(1, "local"), nil)
		for _, owner := range order {
			seedRevision(store, owner, revisionGraph(3, owner))
		}
		requireTargets(t, store, "a")
		if err := store.Save(); err != nil {
			t.Fatal(err)
		}
		store, err = Load(store.path)
		if err != nil {
			t.Fatal(err)
		}
		requireTargets(t, store, "a")
		store.ReplaceSeed("a", nil, nil)
		requireTargets(t, store, "b")
		store.Merge(revisionGraph(4, "new-local"), nil)
		store.ReplaceSeed("b", nil, nil)
		requireTargets(t, store, "new-local")
	}
}

func TestRevalidationMetadataOnlyAndLegacy(t *testing.T) {
	store := New()
	store.ReplaceSeed("legacy", []StoredNode{{URL: freshnessSource, Status: "ok"}}, []StoredEdge{{From: freshnessSource, To: "mark://source/old", Count: 1}})
	if got := store.GetNode(freshnessSource).Observation.Freshness(); got != "unknown" {
		t.Fatalf("legacy freshness = %s", got)
	}
	fetchFn := func(context.Context, string, string) (graph.FetchResult, error) {
		return graph.FetchResult{Status: "ok", Body: "# unchanged", Metadata: map[string]string{"version": "2", "etag": "new-metadata", "content-hash": "same-body", "rel-related": "/new"}}, nil
	}
	result, err := store.Revalidate(t.Context(), nil, fetchFn, fetch.ParseMarkURL)
	if err != nil || result.Fetches != 1 {
		t.Fatalf("revalidation: %+v, %v", result, err)
	}
	requireTargets(t, store, "new")
	if got := store.GetNode(freshnessSource).Observation.Freshness(); got != "fresh" {
		t.Fatalf("validated freshness = %s", got)
	}
	_, edges := store.Snapshot()
	if edges[0].Rel != "related" {
		t.Fatal("metadata-only typed relation lost")
	}
	result, err = store.Revalidate(t.Context(), nil, fetchFn, fetch.ParseMarkURL)
	if err != nil || result.Fetches != 0 {
		t.Fatalf("revalidation cooldown: %+v, %v", result, err)
	}
	store.Merge(revisionGraph(3, "next"), nil)
	requireTargets(t, store, "next")
}

func TestWithdrawnOwnerCannotMakeOlderEvidenceFresh(t *testing.T) {
	store := New()
	seedRevision(store, "a", revisionGraph(3, "older"))
	seedRevision(store, "b", revisionGraph(7, "newer"))
	store.ReplaceSeed("b", nil, nil)
	store.ReplaceSeed("unrelated", nil, nil)
	requireTargets(t, store, "older")
	if node := store.GetNode(freshnessSource); node.Observation.Freshness() != "stale" || node.Observation.HighestRevision != 7 {
		t.Fatalf("withdrawal forgot newer source: %+v", node)
	}
	nodes, edges, err := ParseExportStrict(store.Export())
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	restored.ReplaceSeed("copy", nodes, edges)
	restored.Merge(revisionGraph(4, "still-older"), nil)
	if restored.GetNode(freshnessSource).Observation.Freshness() != "stale" {
		t.Fatal("export lost source high-water mark")
	}
	restored.Merge(revisionGraph(8, "current"), nil)
	if restored.GetNode(freshnessSource).Observation.Freshness() != "fresh" {
		t.Fatal("new source revision did not resolve regression")
	}
}

func TestKnownSeedSupersedesRevisionlessEmptyLocal(t *testing.T) {
	store := New()
	legacy := graph.New()
	legacy.AddNode(&graph.Node{URL: freshnessSource, Status: "not-found"})
	store.Merge(legacy, nil)
	seedRevision(store, "hub", revisionGraph(3, "current"))
	requireTargets(t, store, "current")
	if !store.GetNode(freshnessSource).Seeded {
		t.Fatal("revisionless local absence blocked known seed indefinitely")
	}
}

func TestBacklinkObservationIsAnIndependentCopy(t *testing.T) {
	store := New()
	store.Merge(revisionGraph(3, "target"), nil)
	backlinks := store.BacklinksEnriched("mark://source/target")
	backlinks[0].Observation.Revision = 99
	if store.GetNode(freshnessSource).Observation.Revision != 3 {
		t.Fatal("backlink observation mutated store")
	}
}

func TestExtractionViewPrecedence(t *testing.T) {
	for _, seedFirst := range []bool{false, true} {
		store := New()
		document := revisionGraph(3, "target")
		document.AddEdge(freshnessSource, "https://external.example/doc")
		federation := revisionGraph(3, "target")
		federation.GetNode(freshnessSource).Observation.View = graph.ViewFederation
		if seedFirst {
			seedRevision(store, "hub", federation)
			store.Merge(document, nil)
		} else {
			store.Merge(document, nil)
			seedRevision(store, "hub", federation)
		}
		node := store.GetNode(freshnessSource)
		if node.Observation.Freshness() != "fresh" || node.Observation.View != graph.ViewDocument || store.EdgeCount() != 2 {
			t.Fatalf("equal revision projection conflict: %+v, edges=%d", node, store.EdgeCount())
		}
		newer := revisionGraph(4, "new")
		newer.GetNode(freshnessSource).Observation.View = graph.ViewFederation
		seedRevision(store, "hub", newer)
		requireTargets(t, store, "new")
		if store.GetNode(freshnessSource).Observation.View != graph.ViewFederation {
			t.Fatal("selected source view lost")
		}
	}
	store := New()
	store.Merge(revisionGraph(3, "last-good"), nil)
	unknown := revisionGraph(4, "unverified")
	unknown.GetNode(freshnessSource).Observation.View = ""
	seedRevision(store, "hub", unknown)
	requireTargets(t, store, "last-good")
}

func TestRevalidationBoundsAndLastGood(t *testing.T) {
	store := New()
	for i := range RevalidationLimit + 3 {
		source := fmt.Sprintf("mark://source/%02d.md", i)
		store.ReplaceSeed(source, []StoredNode{{URL: source, Status: "ok"}}, []StoredEdge{{From: source, To: "mark://source/target", Count: 7}})
	}
	var calls atomic.Int32
	failure := errors.New("peer unavailable")
	failed := func(context.Context, string, string) (graph.FetchResult, error) {
		calls.Add(1)
		return graph.FetchResult{}, failure
	}
	result, err := store.Revalidate(t.Context(), nil, failed, fetch.ParseMarkURL)
	if err == nil || result.Fetches != RevalidationLimit || result.Remaining != 3 || store.EdgeCount() != RevalidationLimit+3 {
		t.Fatalf("failed bounded revalidation = %+v, %v, edges=%d", result, err, store.EdgeCount())
	}
	_, edges := store.Snapshot()
	for _, edge := range edges {
		if edge.Count != 7 {
			t.Fatal("last-good occurrence count changed")
		}
	}
	result, err = store.Revalidate(t.Context(), nil, failed, fetch.ParseMarkURL)
	if err == nil || result.Fetches != 3 {
		t.Fatalf("cooldown did not advance to remaining sources: %+v, %v", result, err)
	}
	result, err = store.Revalidate(t.Context(), nil, failed, fetch.ParseMarkURL)
	if err != nil || result.Fetches != 0 || calls.Load() != RevalidationLimit+3 {
		t.Fatalf("failed-source cooldown: %+v, %v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err = store.Revalidate(ctx, nil, failed, fetch.ParseMarkURL)
	if !errors.Is(err, context.Canceled) || result.Fetches != 0 {
		t.Fatalf("cancelled revalidation: %+v, %v", result, err)
	}
}

func TestSourceFreshnessFailureAndCapPreserveKnown(t *testing.T) {
	for _, status := range []string{"error", "partial", "unauthorized"} {
		t.Run(status, func(t *testing.T) {
			store := New()
			initial := revisionGraph(3, "last-good")
			store.Merge(initial, nil)
			failed := revisionGraph(4, "")
			failed.GetNode(freshnessSource).Status = status
			failed.GetNode(freshnessSource).Observation.Complete = false
			store.Merge(failed, nil)
			requireTargets(t, store, "last-good")
			node := store.GetNode(freshnessSource)
			if node.Observation.Revision != 3 || node.Observation.Freshness() != "stale" || node.Etag != "etag-3" {
				t.Fatalf("last-good node = %+v", node)
			}
		})
	}
}

func TestFreshnessProducerConsumerCompatibility(t *testing.T) {
	store := New()
	store.Merge(revisionGraph(9, "target"), nil)
	nodes, edges := store.Snapshot()
	body := BuildExport(time.Now(), nodes, edges)
	parsed, parsedEdges, err := ParseExportStrict(body)
	if err != nil || !reflect.DeepEqual(nodes[0].Observation, parsed[0].Observation) || len(parsedEdges) != 1 {
		t.Fatalf("export round trip: %v", err)
	}
	legacy, _, _ := strings.Cut(body, observationSection)
	oldNodes, oldEdges, err := ParseExportStrict(legacy)
	if err != nil || oldNodes[0].Observation.Freshness() != "unknown" || len(oldEdges) != 1 {
		t.Fatalf("legacy compatibility: %v", err)
	}
	artifacts, err := BuildSnapshotShards(SnapshotManifestPath, SnapshotSlotA, nodes, edges, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		response := protocol.Response{Status: "ok", Body: artifact.Body, Metadata: map[string]string{"version": "73", "content-hash": artifact.ContentHash}}
		got, _, err := verifySnapshotShard(artifact.Ref(73), response)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 0 && got[0].Observation.Revision != 9 {
			t.Fatalf("shard version confused with source revision: %+v", got)
		}
	}
}

func BenchmarkSourceFreshnessRevalidation(b *testing.B) {
	for b.Loop() {
		store := New()
		store.ReplaceSeed("legacy", []StoredNode{{URL: freshnessSource, Status: "ok"}}, nil)
		result, err := store.Revalidate(b.Context(), nil, func(context.Context, string, string) (graph.FetchResult, error) {
			return graph.FetchResult{Status: "ok", Body: "# source", Metadata: map[string]string{"version": "2", "etag": "source-etag", "rel-related": "/target"}}, nil
		}, fetch.ParseMarkURL)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(result.Fetches), "fetches/op")
		b.ReportMetric(float64(result.Bytes), "decoded-bytes/op")
		cached, err := store.Revalidate(b.Context(), nil, nil, fetch.ParseMarkURL)
		if err != nil || cached.Fetches != 0 {
			b.Fatalf("cooldown = %+v, %v", cached, err)
		}
		b.ReportMetric(float64(cached.Fetches), "repeat-fetches/op")
	}
}
