package mcpfmt

import (
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestFormatNeighborhoodGroupsRowsAndLinksSources(t *testing.T) {
	center := "mark://world/center.md"
	page := graphstore.NeighborhoodPage{
		TotalRows: 2,
		Rows: []graphstore.NeighborhoodRow{
			{
				Node: graphstore.StoredNode{URL: "mark://world/a.md", Title: "A"},
				Edges: []graphstore.NeighborhoodEdge{
					{Edge: graph.Edge{From: "mark://world/a.md", To: center, Rel: "depends-on", Count: 1}, Source: graphstore.StoredNode{URL: "mark://world/a.md"}},
					{Edge: graph.Edge{From: center, To: "mark://world/a.md", Label: "A link", Anchor: "proof", Count: 2}, Source: graphstore.StoredNode{URL: center}},
				},
				OmittedEdges: 3,
			},
			{Node: graphstore.StoredNode{URL: "mark://world/b.md"}},
		},
		NextCursor: "next",
	}

	got := FormatNeighborhood(center, page)
	for _, want := range []string{
		"## Relations (2 documents)",
		"- [A](mark://world/a.md)",
		"incoming [depends-on]; [source](mark://world/a.md)",
		"outgoing [link]; [source](mark://world/center.md#proof) \"A link\" x2",
		"- [mark://world/b.md](mark://world/b.md)",
		"+3 more edges",
		"next-cursor: next",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestNeighborhoodRequested(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want bool
	}{
		{"URL only", map[string]any{"url": "/doc.md"}, false},
		{"verbose metadata", map[string]any{"url": "/doc.md", "verbose": true}, false},
		{"direction", map[string]any{"direction": "both"}, true},
		{"all relations", map[string]any{"relations": []string{}}, true},
		{"page size", map[string]any{"page_size": 10}, true},
		{"continuation", map[string]any{"cursor": "next"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Arguments = tc.args
			if got := NeighborhoodRequested(&req); got != tc.want {
				t.Fatalf("requested=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestFormatExploreBacklinksKeepsEvidenceAndCapsRows(t *testing.T) {
	rows := make([]graphstore.BacklinkEntry, 12)
	for i := range rows {
		rows[i] = graphstore.BacklinkEntry{URL: fmt.Sprintf("mark://world/%02d.md", i)}
	}
	rows[0].Title, rows[0].Rel, rows[0].Anchor = "Decision", "supersedes", "decision"
	rows[0].Label, rows[0].Count = "Old decision", 2
	text := FormatExploreBacklinks(rows)
	for _, want := range []string{"## Backlinks (12)", "[Decision](mark://world/00.md)", "[supersedes]", "#decision", "Old decision", "x2", "freshness: unknown", "+2 more backlinks"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if strings.Contains(text, "mark://world/10.md") || strings.Contains(text, "[source](") || strings.Count(text, "\n- ") != 10 {
		t.Fatalf("default backlinks exceeded their output contract:\n%s", text)
	}
	if empty := FormatExploreBacklinks(nil); empty != "## Backlinks (0)\n(none recorded)\n" {
		t.Fatalf("empty graph claimed absence: %q", empty)
	}
}

// The store keys by identity; a caller may name the document as it was typed.
// Direction must not depend on the spelling.
func TestFormatNeighborhoodDirectionIgnoresHowTheCenterWasSpelled(t *testing.T) {
	page := graphstore.NeighborhoodPage{TotalRows: 1, Rows: []graphstore.NeighborhoodRow{{
		Node:  graphstore.StoredNode{URL: "mark://team-a/src.md"},
		Edges: []graphstore.NeighborhoodEdge{{Edge: graph.Edge{From: "mark://team-a/src.md", To: "mark://team-a/x.md"}}},
	}}}
	for _, center := range []string{"mark://team-a/x.md", "mark://Team-A/x.md", "mark://team-a:6309/x.md"} {
		if got := FormatNeighborhood(center, page); !strings.Contains(got, "incoming") || strings.Contains(got, "outgoing") {
			t.Errorf("center %q:\n%s\nwant the edge into the document read as incoming", center, got)
		}
	}
}
