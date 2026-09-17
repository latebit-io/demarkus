package mcpfmt

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/mark3labs/mcp-go/mcp"
)

const neighborhoodPageSize = 10

// ExploreDescription keeps default and explicit navigation behavior consistent.
const ExploreDescription = "Orient around one document: outline, outbound links, backlinks, siblings (10 each). Relation query arguments request grouped, paginated neighborhoods."

// RevalidatesBacklinks decides when explore pays for source revalidation:
// only relation queries that read incoming edges. Default explore reads the cache.
func RevalidatesBacklinks(req *mcp.CallToolRequest) bool {
	return NeighborhoodRequested(req) && NeighborhoodOptions(req).Direction != graphstore.NeighborhoodOutgoing
}

// NeighborhoodRequested preserves lean defaults for existing URL-only callers.
func NeighborhoodRequested(req *mcp.CallToolRequest) bool {
	args := req.GetArguments()
	for _, name := range []string{"direction", "relations", "page_size", "cursor"} {
		if _, exists := args[name]; exists {
			return true
		}
	}
	return false
}

// FormatExploreBacklinks caps default output without repeating outbound relations.
func FormatExploreBacklinks(backlinks []graphstore.BacklinkEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Backlinks (%d)\n", len(backlinks))
	if len(backlinks) == 0 {
		b.WriteString("(none recorded)\n")
	}
	for _, link := range backlinks[:min(len(backlinks), neighborhoodPageSize)] {
		annotation := graph.EdgeAnnotation(link.Rel, link.Label, link.Anchor, link.Count) + link.Observation.Annotation()
		if link.Title != "" {
			fmt.Fprintf(&b, "- [%s](%s)%s\n", link.Title, link.URL, annotation)
		} else {
			fmt.Fprintf(&b, "- %s%s\n", link.URL, annotation)
		}
	}
	if omitted := len(backlinks) - neighborhoodPageSize; omitted > 0 {
		fmt.Fprintf(&b, "+%d more backlinks\n", omitted)
	}
	return b.String()
}

// NeighborhoodParams declares the shared bounded graph query arguments.
func NeighborhoodParams() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("direction",
			mcp.Description("incoming, outgoing, or both (default both)"),
			mcp.Enum("incoming", "outgoing", "both"),
		),
		mcp.WithArray("relations",
			mcp.Description("relation predicates to include; empty string is body links; omit for all"),
			mcp.WithStringItems(), mcp.MaxItems(graphstore.MaxNeighborhoodEdgesPerRow), mcp.UniqueItems(true),
		),
		mcp.WithNumber("page_size",
			mcp.Description("documents per page, 1-100 (default 10)"),
			mcp.Min(1), mcp.Max(graphstore.MaxNeighborhoodPageSize), mcp.MultipleOf(1),
		),
		mcp.WithString("cursor", mcp.Description(cursorDesc)),
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
