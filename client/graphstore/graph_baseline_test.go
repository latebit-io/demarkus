package graphstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
			fetchDoc := func(_, path string) (graph.FetchResult, error) {
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
				if err != nil && !errors.Is(err, context.Canceled) {
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
