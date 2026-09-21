package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/links"
)

type fetchFunc func(context.Context, links.Target) (FetchResult, error)

func (f fetchFunc) Fetch(ctx context.Context, target links.Target) (FetchResult, error) {
	return f(ctx, target)
}

func TestCrawlBounds(t *testing.T) {
	var body strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&body, "[child](/child-%d.md)\n", i)
	}
	for _, tt := range []struct {
		name      string
		opts      CrawlOptions
		reason    string
		wantNodes int
		wantEdges int
	}{
		{"node-cap", CrawlOptions{MaxDepth: 1, MaxNodes: 10, Workers: 3}, ReasonNodeCap, 10, 5000},
		{"frontier-cap", CrawlOptions{MaxDepth: 1, MaxFrontier: 4, Workers: 2}, ReasonFrontierCap, 5, 5000},
		{"depth-boundary", CrawlOptions{MaxDepth: 0, MaxNodes: 1}, "", 1, 5000},
		{"output-cap", CrawlOptions{MaxDepth: 1, MaxOutputBytes: 1024}, ReasonOutputCap, 1, -1},
		{"byte-cap", CrawlOptions{MaxDepth: 1, MaxFetchBytes: 100}, ReasonByteCap, 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newMockFetcher()
			f.add("host:6309", "/root.md", body.String())
			g, err := Crawl(t.Context(), "mark://host/root.md", f, tt.opts)
			if tt.reason == "" {
				if err != nil || !g.Outcome.Complete {
					t.Fatalf("complete requested scope: %v, %+v", err, g.Outcome)
				}
			} else if !errors.Is(err, ErrIncomplete) || !slices.Contains(g.Outcome.Reasons, tt.reason) {
				t.Fatalf("outcome = %+v, err = %v, want %s", g.Outcome, err, tt.reason)
			}
			if g.NodeCount() != tt.wantNodes || (tt.wantEdges >= 0 && g.EdgeCount() != tt.wantEdges) {
				t.Fatalf("nodes/edges = %d/%d, want %d/%d", g.NodeCount(), g.EdgeCount(), tt.wantNodes, tt.wantEdges)
			}
			opts := tt.opts
			opts.applyDefaults()
			if len(f.calls) != tt.wantNodes || g.Outcome.Admitted > opts.MaxNodes || g.Outcome.PeakWorkers > opts.Workers || g.Outcome.PeakFrontier > opts.MaxFrontier {
				t.Fatalf("work exceeded limits: %+v, fetches=%d", g.Outcome, len(f.calls))
			}
			if len(Summary(g, "mark://host/root.md")) > opts.MaxOutputBytes || g.Outcome.FetchedBytes > opts.MaxFetchBytes {
				t.Fatal("byte limits exceeded")
			}
			if tt.name == "output-cap" && (g.EdgeCount() == 0 || !g.GetNode("mark://host/root.md").Incomplete) {
				t.Fatal("output cap must preserve some edges and mark the source incomplete")
			}
		})
	}
}

func TestCrawlExactNodeCapIsComplete(t *testing.T) {
	f := newMockFetcher()
	f.add("host:6309", "/root.md", "[child](/child.md)")
	f.add("host:6309", "/child.md", "# Child")
	g, err := Crawl(t.Context(), "mark://host/root.md", f, CrawlOptions{MaxDepth: 1, MaxNodes: 2})
	if err != nil || !g.Outcome.Complete || g.NodeCount() != 2 {
		t.Fatalf("exact fit is complete: %+v, %v", g.Outcome, err)
	}
}

func TestCrawlMidFetchCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	f := fetchFunc(func(ctx context.Context, target links.Target) (FetchResult, error) {
		path := target.Path
		if path == "/root.md" {
			return FetchResult{Status: "ok", Body: "[child](/child.md)"}, nil
		}
		close(started)
		<-ctx.Done()
		return FetchResult{}, ctx.Err()
	})
	done := make(chan struct{})
	var g *Graph
	var err error
	go func() {
		g, err = Crawl(ctx, "mark://host/root.md", f, CrawlOptions{MaxDepth: 1})
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("child fetch not started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("crawl did not join cancelled worker")
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrIncomplete) || g.Outcome.Complete || g.EdgeCount() != 1 || g.Outcome.Failures != 0 {
		t.Fatalf("cancelled outcome lost observations: %+v, %v", g.Outcome, err)
	}
}

func TestCrawlFailedPeers(t *testing.T) {
	for _, status := range []string{"", "unauthorized", "server-error", "rate-limited", "external", "transport"} {
		t.Run(status, func(t *testing.T) {
			f := fetchFunc(func(_ context.Context, target links.Target) (FetchResult, error) {
				path := target.Path
				if path == "/root.md" {
					return FetchResult{Status: "ok", Body: "[good](/good.md) [bad](mark://peer/bad.md)"}, nil
				}
				if path == "/good.md" {
					return FetchResult{Status: "ok", Body: "[boundary](/outside.md)"}, nil
				}
				if status == "transport" {
					return FetchResult{}, errors.New("peer disconnected")
				}
				return FetchResult{Status: status}, nil
			})
			g, err := Crawl(t.Context(), "mark://host/root.md", f, CrawlOptions{MaxDepth: 1})
			if !errors.Is(err, ErrIncomplete) || g.Outcome.Failures != 1 || g.EdgeCount() != 3 || g.NodeCount() != 3 {
				t.Fatalf("failed peer lost valid neighborhood: %+v, %v", g.Outcome, err)
			}
			if status == "transport" && !strings.Contains(Summary(g, "mark://host/root.md"), "peer disconnected") {
				t.Fatal("fetch error was swallowed")
			}
		})
	}
}

func TestCrawlWorkerCeiling(t *testing.T) {
	f := newMockFetcher()
	var body strings.Builder
	for i := range 100 {
		fmt.Fprintf(&body, "[child](/%d.md)\n", i)
	}
	f.add("host:6309", "/root.md", body.String())
	g, err := Crawl(t.Context(), "mark://host/root.md", f, CrawlOptions{MaxDepth: 1, Workers: 1000000})
	if err != nil || g.Outcome.PeakWorkers != 32 {
		t.Fatalf("worker bound: %+v, %v", g.Outcome, err)
	}
}

func TestCrawlRetainsMinimalRootWithinOutputBudget(t *testing.T) {
	const root = "mark://host/root.md"
	f := newMockFetcher()
	f.add("host:6309", "/root.md", "# "+strings.Repeat("large title ", 200))
	g, err := Crawl(t.Context(), root, f, CrawlOptions{MaxOutputBytes: 1024})
	if !errors.Is(err, ErrIncomplete) || !slices.Contains(g.Outcome.Reasons, ReasonOutputCap) {
		t.Fatalf("want output-cap, got %v", err)
	}
	if n := g.GetNode(root); n == nil || !n.Incomplete || n.Depth != 0 || n.Title != "" {
		t.Fatalf("root observation = %+v, want minimal incomplete root", n)
	}
	if len(Summary(g, root)) > 1024 || len(f.calls) != 1 {
		t.Fatal("root retention exceeded output or fetch budget")
	}
}

func TestCrawlRejectsUnrepresentableRootBeforeFetch(t *testing.T) {
	root := "mark://host/" + strings.Repeat("x", 500)
	f := newMockFetcher()
	g, err := Crawl(t.Context(), root, f, CrawlOptions{MaxOutputBytes: 1024})
	if err == nil || g != nil || len(f.calls) != 0 {
		t.Fatalf("unrepresentable root: graph=%v err=%v fetches=%d", g, err, len(f.calls))
	}
}

func TestCrawlRetainsMinimalChildWithinOutputBudget(t *testing.T) {
	const root = "mark://host/root.md"
	const child = "mark://host/child.md"
	f := newMockFetcher()
	f.add("host:6309", "/root.md", "[child](/child.md)")
	f.add("host:6309", "/child.md", "# "+strings.Repeat("large title ", 200))
	g, err := Crawl(t.Context(), root, f, CrawlOptions{MaxDepth: 1, MaxOutputBytes: 1024})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatal("expected partial traversal")
	}
	if n := g.GetNode(child); n == nil || !n.Incomplete || n.Depth != 1 {
		t.Fatalf("child observation = %+v, want depth 1 incomplete node", n)
	}
	if g.EdgeCount() != 1 || len(Summary(g, root)) > 1024 {
		t.Fatal("fallback lost edge or exceeded output budget")
	}
}

func TestCrawlWarningPreservesOtherErrors(t *testing.T) {
	outcome := &CrawlOutcome{Reasons: []string{ReasonNodeCap}}
	saveErr := errors.New("disk full")
	closeErr := errors.New("close failed")
	wrapped := fmt.Errorf("save graph: %w", saveErr)
	for _, tt := range []struct {
		name string
		err  error
		want []error
	}{
		{"nil", nil, nil},
		{"outcome", outcome, nil},
		{"outcomes only", errors.Join(outcome, outcome), nil},
		{"plain error", saveErr, []error{saveErr}},
		{"wrapped error", wrapped, []error{wrapped, saveErr}},
		{"joined", errors.Join(outcome, wrapped), []error{wrapped, saveErr}},
		{"nested join", errors.Join(errors.Join(outcome, wrapped), closeErr), []error{wrapped, saveErr, closeErr}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			warning := CrawlWarning(tt.err, outcome)
			if len(tt.want) == 0 {
				if warning != nil {
					t.Fatalf("unexpected warning: %v", warning)
				}
				return
			}
			if warning == nil || strings.Contains(warning.Error(), "outcome:") {
				t.Fatalf("wrong warning: %v", warning)
			}
			for _, want := range tt.want {
				if !errors.Is(warning, want) {
					t.Errorf("lost %v from %v", want, warning)
				}
			}
		})
	}
}
