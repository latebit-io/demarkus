package graph

import (
	"strings"
	"testing"
)

// The edge annotation literals are tool output on every surface: mark_graph
// prints Summary as is, and the broker's fidelity test pins the same strings.
func TestSummaryEdgeAnnotations(t *testing.T) {
	g := New()
	g.AddNode(&Node{URL: "mark://w/a.md", Status: "ok"})
	g.AddEdgeInfo(Edge{From: "mark://w/a.md", To: "mark://w/b.md", Rel: "supersedes"})
	g.AddEdgeInfo(Edge{From: "mark://w/a.md", To: "mark://w/c.md", Label: "Getting started", Anchor: "intro", Count: 3})

	out := Summary(g, "mark://w/a.md")
	for _, want := range []string{
		"  mark://w/a.md -> mark://w/b.md [supersedes]",
		"  mark://w/a.md -> mark://w/c.md (\"Getting started\", #intro, x3)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Summary missing %q\n---\n%s", want, out)
		}
	}
}
