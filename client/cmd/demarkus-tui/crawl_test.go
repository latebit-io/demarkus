package main

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/graph"
)

func TestPartialCrawlStaysVisible(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host/root.md", Status: "ok", Title: "Root"})
	g.Outcome = &graph.CrawlOutcome{MaxDepth: 10, Reasons: []string{graph.ReasonNodeCap}}
	m := model{viewMode: viewGraph, crawling: true, crawlSeq: 1, width: 100}
	updated, _ := m.handleCrawlResult(crawlResult{graph: g, err: g.Outcome, url: "mark://host/root.md", seq: 1})
	m = updated.(model)
	if m.viewMode != viewGraph || m.crawling || len(m.graphNodes) != 1 {
		t.Fatal("partial graph replaced by error screen")
	}
	view := m.renderCurrentGraphSubView()
	for _, want := range []string{"outcome: partial", "node-cap", "Root"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q: %s", want, view)
		}
	}
}

func TestCancelCrawlInvalidatesLateResult(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	m := model{crawlCancel: cancel, crawlSeq: 1, crawling: true}
	m.cancelCrawl()
	if ctx.Err() == nil || m.crawling || m.crawlSeq != 2 {
		t.Fatal("crawl cancellation failed")
	}
	updated, _ := m.handleCrawlResult(crawlResult{graph: graph.New(), seq: 1})
	if updated.(model).graphData != nil {
		t.Fatal("stale crawl restored after cancellation")
	}
}
