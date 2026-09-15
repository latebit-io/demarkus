package mcpfmt

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
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
