package main

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/graph"
)

// A crawled document's title is untrusted: its escapes never reach the terminal raw.
func TestCrawlLogRevealsHiddenText(t *testing.T) {
	const hostile = "Docs\x1b]0;owned\x07"
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://h/a.md", Title: hostile, Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://h/b.md", Status: "ok"})
	for _, line := range []string{crawlNodeLine(g.GetNode("mark://h/a.md")), crawlEdgeLine(g, "mark://h/a.md", "mark://h/b.md")} {
		if strings.ContainsAny(line, "\x1b\x07") || !strings.Contains(line, "Docs<U+001B>]0;owned<U+0007>") {
			t.Errorf("line = %q, want the escape revealed", line)
		}
	}
	if line := crawlEdgeLine(g, "mark://h/a.md", "mark://h/b.md"); !strings.HasSuffix(line, " -> mark://h/b.md") {
		t.Errorf("edge line = %q, want the untitled node by URL", line)
	}
}
