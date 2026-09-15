package mcpfmt

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/mark3labs/mcp-go/mcp"
)

const neighborhoodPageSize = 10

// NeighborhoodParams declares the shared bounded graph query arguments.
func NeighborhoodParams() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("direction",
			mcp.Description("relations relative to url: incoming, outgoing, or both (default both)"),
			mcp.Enum("incoming", "outgoing", "both"),
		),
		mcp.WithArray("relations",
			mcp.Description("exact relation predicates; empty string selects body links; omit for all"),
			mcp.WithStringItems(), mcp.MaxItems(graphstore.MaxNeighborhoodEdgesPerRow), mcp.UniqueItems(true),
		),
		mcp.WithNumber("page_size",
			mcp.Description("neighbor documents per page, 1-100 (default 10)"),
			mcp.Min(1), mcp.Max(graphstore.MaxNeighborhoodPageSize), mcp.MultipleOf(1),
		),
		mcp.WithString("cursor", mcp.Description("continuation cursor from prior mark_explore result")),
	}
}

// NeighborhoodOptions reads the shared graph query arguments.
func NeighborhoodOptions(req *mcp.CallToolRequest) graphstore.NeighborhoodOptions {
	return graphstore.NeighborhoodOptions{
		Direction: graphstore.NeighborhoodDirection(req.GetString("direction", "both")),
		Relations: req.GetStringSlice("relations", nil),
		PageSize:  req.GetInt("page_size", neighborhoodPageSize),
		Cursor:    req.GetString("cursor", ""),
	}
}

// FormatNeighborhood renders grouped relation rows with source evidence.
func FormatNeighborhood(center string, page graphstore.NeighborhoodPage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Relations (%d documents)\n", page.TotalRows)
	if len(page.Rows) == 0 {
		b.WriteString("(none recorded)\n")
	}
	for i := range page.Rows {
		row := &page.Rows[i]
		label := row.Node.Title
		if label == "" {
			label = row.Node.URL
		}
		fmt.Fprintf(&b, "- [%s](%s)\n", label, row.Node.URL)
		for j := range row.Edges {
			writeNeighborhoodEdge(&b, center, &row.Edges[j])
		}
		if row.OmittedEdges > 0 {
			fmt.Fprintf(&b, "  - +%d more edges\n", row.OmittedEdges)
		}
	}
	if page.NextCursor != "" {
		fmt.Fprintf(&b, "next-cursor: %s\n", page.NextCursor)
	}
	return b.String()
}

func writeNeighborhoodEdge(b *strings.Builder, center string, item *graphstore.NeighborhoodEdge) {
	edge := &item.Edge
	direction := "outgoing"
	if edge.To == center {
		direction = "incoming"
	}
	relation := edge.Rel
	if relation == "" {
		relation = "link"
	}
	source := edge.From
	if edge.Anchor != "" {
		source += "#" + edge.Anchor
	}
	fmt.Fprintf(b, "  - %s [%s]; [source](%s)", direction, relation, source)
	if edge.Label != "" {
		fmt.Fprintf(b, " %s", strconv.Quote(edge.Label))
	}
	if edge.Count > 1 {
		fmt.Fprintf(b, " x%d", edge.Count)
	}
	b.WriteString(item.Source.Observation.Annotation())
	b.WriteByte('\n')
}
