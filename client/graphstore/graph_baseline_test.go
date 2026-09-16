package graphstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
)

const baselineTarget = "mark://fixture.example/target.md"

func baselineGraph(edges, fanIn int) *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{URL: baselineTarget, Title: "Target", Status: "ok"})
	for i := range edges {
		source := fmt.Sprintf("mark://fixture.example/source-%06d.md", i)
		target := baselineTarget
		if i >= fanIn {
			target = source
		}
		g.AddNode(&graph.Node{URL: source, Title: "Source", Status: "ok"})
		g.AddEdgeInfo(graph.Edge{From: source, To: target, Rel: "references", Anchor: "evidence", Count: 1})
	}
	return g
}

func BenchmarkGraphBaselineBacklinks(b *testing.B) {
	for _, edges := range []int{1000, 10000, 100000} {
		for _, fanIn := range []int{1, 1000} {
			b.Run(fmt.Sprintf("edges=%d/fan-in=%d", edges, fanIn), func(b *testing.B) {
				store := New()
				store.Merge(baselineGraph(edges, fanIn), nil)
				for b.Loop() {
					if got := len(store.BacklinksEnriched(baselineTarget)); got != fanIn {
						b.Fatalf("backlinks=%d, want %d", got, fanIn)
					}
				}
				b.ReportMetric(float64(fanIn), "rows/op")
			})
		}
	}
}

// Restore is a new Store over a warm filesystem cache, not cold disk latency.
func BenchmarkGraphBaselineRestore(b *testing.B) {
	for _, edges := range []int{1000, 100000} {
		b.Run(fmt.Sprintf("edges=%d", edges), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "graph.json")
			store, err := Load(path)
			if err != nil {
				b.Fatal(err)
			}
			store.Merge(baselineGraph(edges, 1), nil)
			if err := store.Save(); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				restored, err := Load(path)
				if err != nil {
					b.Fatal(err)
				}
				if got := len(restored.BacklinksEnriched(baselineTarget)); got != 1 {
					b.Fatalf("restored backlinks=%d, want 1", got)
				}
			}
		})
	}
}

func BenchmarkGraphBaselineNeighborhood(b *testing.B) {
	for _, edges := range []int{1000, 10000, 100000} {
		for _, fanIn := range []int{1, 1000} {
			b.Run(fmt.Sprintf("edges=%d/fan-in=%d", edges, fanIn), func(b *testing.B) {
				store := New()
				store.Merge(baselineGraph(edges, fanIn), nil)
				wantRows := min(fanIn, MaxNeighborhoodPageSize)
				assertBaselineNeighborhood(b, store, fanIn)
				for b.Loop() {
					page, err := store.Neighborhood(baselineTarget, NeighborhoodOptions{
						Direction: NeighborhoodIncoming,
						Relations: []string{"references"},
						PageSize:  MaxNeighborhoodPageSize,
					})
					if err != nil {
						b.Fatal(err)
					}
					if len(page.Rows) != wantRows || page.TotalRows != fanIn {
						b.Fatalf("neighborhood rows=%d total=%d, want %d total=%d", len(page.Rows), page.TotalRows, wantRows, fanIn)
					}
				}
				b.ReportMetric(float64(wantRows), "rows/op")
			})
		}
	}
}

func BenchmarkGraphBaselineNeighborhoodRestore(b *testing.B) {
	for _, edges := range []int{1000, 100000} {
		b.Run(fmt.Sprintf("edges=%d", edges), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "graph.json")
			store, err := Load(path)
			if err != nil {
				b.Fatal(err)
			}
			store.Merge(baselineGraph(edges, 1), nil)
			if err := store.Save(); err != nil {
				b.Fatal(err)
			}
			assertBaselineNeighborhood(b, store, 1)
			for b.Loop() {
				restored, err := Load(path)
				if err != nil {
					b.Fatal(err)
				}
				page, err := restored.Neighborhood(baselineTarget, NeighborhoodOptions{Direction: NeighborhoodIncoming})
				if err != nil || page.TotalRows != 1 {
					b.Fatalf("restored neighborhood=%+v, err=%v", page, err)
				}
			}
		})
	}
}

func BenchmarkGraphBaselineObserveDocument(b *testing.B) {
	for _, mode := range []string{"stable", "changing"} {
		for _, links := range []int{1, 100} {
			b.Run(fmt.Sprintf("%s/links=%d", mode, links), func(b *testing.B) {
				var body strings.Builder
				body.WriteString("# Source\n\n")
				for i := range links {
					fmt.Fprintf(&body, "[target](mark://fixture.example/target-%03d.md)\n", i)
				}
				store := New()
				version := 1
				metadata := map[string]string{"version": "1", "etag": "stable"}
				store.ObserveDocument("mark://fixture.example/source.md", graph.FetchResult{Status: "ok", Body: body.String(), Metadata: metadata})
				assertObservedDocument(b, store, links)
				if mode == "stable" {
					store.mu.Lock()
					local := store.localSources["mark://fixture.example/source.md"]
					local.Node.Observation.ObservedAt = time.Time{}
					local.Node.Observation.AttemptedAt = time.Time{}
					store.localSources["mark://fixture.example/source.md"] = local
					store.nodes["mark://fixture.example/source.md"].Observation = local.Node.Observation
					store.mu.Unlock()
				}
				for b.Loop() {
					if mode == "changing" {
						version++
						metadata = map[string]string{"version": strconv.Itoa(version), "etag": strconv.Itoa(version)}
					}
					if !store.ObserveDocument("mark://fixture.example/source.md", graph.FetchResult{
						Status: "ok", Body: body.String(), Metadata: metadata,
					}) {
						b.Fatal("observation did not update the store")
					}
				}
				assertObservedDocument(b, store, links)
				node := store.GetNode("mark://fixture.example/source.md")
				if mode == "stable" && (node.Observation.ObservedAt.IsZero() || node.Observation.Freshness() != "fresh") {
					b.Fatalf("stable observation did not refresh: %+v", node.Observation)
				}
				if mode == "changing" && (node.Observation.Revision != version || node.Etag != strconv.Itoa(version)) {
					b.Fatalf("changing observation = version %d etag %q, want %d", node.Observation.Revision, node.Etag, version)
				}
				b.ReportMetric(float64(links), "links/op")
			})
		}
	}
}

func BenchmarkGraphBaselineSave(b *testing.B) {
	for _, edges := range []int{1000, 100000} {
		b.Run(fmt.Sprintf("edges=%d", edges), func(b *testing.B) {
			store, err := Load(filepath.Join(b.TempDir(), "graph.json"))
			if err != nil {
				b.Fatal(err)
			}
			store.Merge(baselineGraph(edges, 1), nil)
			for b.Loop() {
				if err := store.Save(); err != nil {
					b.Fatal(err)
				}
			}
			restored, err := Load(store.path)
			if err != nil {
				b.Fatal(err)
			}
			if restored.EdgeCount() != edges {
				b.Fatalf("saved graph edges=%d, want %d", restored.EdgeCount(), edges)
			}
		})
	}
}

func assertBaselineNeighborhood(tb testing.TB, store *Store, total int) {
	tb.Helper()
	page, err := store.Neighborhood(baselineTarget, NeighborhoodOptions{
		Direction: NeighborhoodIncoming, Relations: []string{"references"}, PageSize: MaxNeighborhoodPageSize,
	})
	if err != nil {
		tb.Fatal(err)
	}
	wantRows := min(total, MaxNeighborhoodPageSize)
	if len(page.Rows) != wantRows || page.TotalRows != total || page.Rows[0].Node.URL != "mark://fixture.example/source-000000.md" {
		tb.Fatalf("neighborhood rows=%d total=%d first=%q", len(page.Rows), page.TotalRows, page.Rows[0].Node.URL)
	}
	last := &page.Rows[len(page.Rows)-1]
	wantLast := fmt.Sprintf("mark://fixture.example/source-%06d.md", wantRows-1)
	if last.Node.URL != wantLast || len(last.Edges) != 1 || last.Edges[0].Edge.Rel != "references" || last.Edges[0].Edge.To != baselineTarget {
		tb.Fatalf("last neighborhood row = %+v, want URL %q", last, wantLast)
	}
	filtered, err := store.Neighborhood(baselineTarget, NeighborhoodOptions{
		Direction: NeighborhoodIncoming, Relations: []string{"depends-on"}, PageSize: MaxNeighborhoodPageSize,
	})
	if err != nil {
		tb.Fatal(err)
	}
	if len(filtered.Rows) != 0 || filtered.TotalRows != 0 {
		tb.Fatalf("relation filter returned rows=%d total=%d", len(filtered.Rows), filtered.TotalRows)
	}
}

func assertObservedDocument(tb testing.TB, store *Store, links int) {
	tb.Helper()
	page, err := store.Neighborhood("mark://fixture.example/source.md", NeighborhoodOptions{
		Direction: NeighborhoodOutgoing, PageSize: MaxNeighborhoodPageSize,
	})
	if err != nil {
		tb.Fatal(err)
	}
	if len(page.Rows) != links || page.TotalRows != links || page.Rows[0].Node.URL != "mark://fixture.example/target-000.md" {
		tb.Fatalf("observed neighborhood rows=%d total=%d first=%q", len(page.Rows), page.TotalRows, page.Rows[0].Node.URL)
	}
	wantLast := fmt.Sprintf("mark://fixture.example/target-%03d.md", links-1)
	if page.Rows[len(page.Rows)-1].Node.URL != wantLast {
		tb.Fatalf("last observed row = %q, want %q", page.Rows[len(page.Rows)-1].Node.URL, wantLast)
	}
}

func BenchmarkGraphBaselineCrawlOutcomes(b *testing.B) {
	var body strings.Builder
	body.WriteString("# Root\n")
	for i := range 100 {
		fmt.Fprintf(&body, "[child](/child-%d.md)\n", i)
	}
	root := body.String()
	for _, name := range []string{"complete", "pre-cancelled", "node-cap"} {
		b.Run(name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(b.Context())
			defer cancel()
			if name == "pre-cancelled" {
				cancel()
			}
			opts := CrawlOptions{MaxDepth: 1, Workers: 1}
			if name == "node-cap" {
				opts.MaxNodes = 10
			}
			var fetches atomic.Int64
			fetchDoc := func(_ context.Context, _, path string) (graph.FetchResult, error) {
				fetches.Add(1)
				body := "# Child\n"
				if path == "/index.md" {
					body = root
				}
				return graph.FetchResult{Status: "ok", Body: body}, nil
			}
			var silent, nodes, edges int
			for b.Loop() {
				var store *Store
				g, err := store.CrawlAndPersist(ctx, "mark://fixture.example/index.md", fetchDoc, fetch.ParseMarkURL, opts)
				if err != nil && !errors.Is(err, graph.ErrIncomplete) {
					b.Fatalf("crawl: %v", err)
				}
				if name == "complete" && err != nil {
					b.Fatalf("uncancelled crawl: %v", err)
				}
				if name == "complete" && (g == nil || g.NodeCount() != 101 || len(g.GetEdges()) != 100) {
					b.Fatal("complete crawl must retain all 101 nodes and 100 edges")
				}
				if name != "complete" && err == nil {
					silent++
				}
				if g != nil {
					nodes += g.NodeCount()
					edges += len(g.GetEdges())
				}
			}
			b.ReportMetric(float64(silent)/float64(b.N), "silent-incomplete/op")
			b.ReportMetric(float64(fetches.Load())/float64(b.N), "fetches/op")
			b.ReportMetric(float64(nodes)/float64(b.N), "nodes/op")
			b.ReportMetric(float64(edges)/float64(b.N), "edges/op")
		})
	}
}
