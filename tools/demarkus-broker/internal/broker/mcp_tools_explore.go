package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// exploreSectionCap bounds each section of the neighborhood card.
// Overflow is reported honestly as "+N more". Matches the local
// demarkus-mcp value.
const exploreSectionCap = 10

// handleMarkExplore implements the mark_explore tool: one call returns a
// neighborhood card for a document — outline head (heading tree + opening
// paragraph), outbound links, recorded backlinks, and the sibling listing
// of its parent directory. Mirrors client/cmd/demarkus-mcp markExplore;
// the broker differences are the world-name URL shape and that backlinks
// come from the broker's ephemeral graph store.
func (g *mcpGateway) handleMarkExplore(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	docURL, _, _ := strings.Cut(raw, "#")

	worldName, path, err := parseToolURL(docURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	result, err := g.dispatcher.Fetch(worldName, path, "")
	if err != nil {
		return g.toolErrorFor("explore", worldName, err), nil
	}
	if result.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultText(formatToolResult(result, "version", "modified", "etag")), nil
	}
	body := result.Response.Body

	// Binary/non-UTF-8 body: an outline over it is garbage; return a notice.
	if mdoutline.BinaryBody(body) {
		return mcp.NewToolResultText(formatToolResultWith(result, mdoutline.NonMarkdownNotice(len(body)),
			map[string]string{"mode": "binary"}, "version", "modified", "etag")), nil
	}

	var b strings.Builder

	b.WriteString("## Outline\n")
	if tree := mdoutline.Outline(body); tree != "" {
		mdoutline.CappedList(&b, strings.Split(strings.TrimRight(tree, "\n"), "\n"), exploreSectionCap, "headings")
	} else {
		b.WriteString("(no headings)\n")
	}
	if para := mdoutline.OpeningParagraph(body); para != "" {
		b.WriteString("\n## Opening\n")
		b.WriteString(para)
		b.WriteString("\n")
	}

	out := mdoutline.LinkLines(body)
	fmt.Fprintf(&b, "\n## Outbound links (%d)\n", len(out))
	if len(out) == 0 {
		b.WriteString("(none)\n")
	} else {
		mdoutline.CappedList(&b, out, exploreSectionCap, "links")
	}

	g.seedGraphStore()
	g.writeBacklinksSection(&b, docURL)
	g.writeSiblingsSection(&b, worldName, path)

	fmt.Fprintf(&b, "\nfetch %s#<anchor> for a section; mark_fetch force=true for the full body\n", docURL)

	extra := map[string]string{
		"size": fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1),
	}
	return mcp.NewToolResultText(formatToolResultWith(result, b.String(), extra, "version", "modified", "etag")), nil
}

// writeBacklinksSection appends the backlinks card section from the
// broker's ephemeral graph store. An empty store degrades to a note,
// never an error. The store is keyed by full mark://{worldName}/{path}
// URLs — the same shape mark_graph crawls record.
func (g *mcpGateway) writeBacklinksSection(b *strings.Builder, docURL string) {
	backlinks := g.graphStore.BacklinksEnriched(docURL)
	fmt.Fprintf(b, "\n## Backlinks (%d)\n", len(backlinks))
	if len(backlinks) == 0 {
		b.WriteString("(none recorded; run mark_graph to populate; the broker's graph store is per-pod and resets on restart)\n")
		return
	}
	lines := make([]string, len(backlinks))
	for i, bl := range backlinks {
		ann := graph.EdgeAnnotation(bl.Rel, bl.Label, bl.Anchor, bl.Count)
		if bl.Title != "" {
			lines[i] = fmt.Sprintf("- [%s](%s)%s", bl.Title, bl.URL, ann)
		} else {
			lines[i] = "- " + bl.URL + ann
		}
	}
	mdoutline.CappedList(b, lines, exploreSectionCap, "backlinks")
}

// writeSiblingsSection appends the sibling listing of the document's
// parent directory. A failed LIST degrades to a note, never an error.
func (g *mcpGateway) writeSiblingsSection(b *strings.Builder, worldName, path string) {
	dir := path[:strings.LastIndex(path, "/")+1]
	self := path[strings.LastIndex(path, "/")+1:]

	fmt.Fprintf(b, "\n## Siblings in %s", dir)
	listing, err := g.dispatcher.List(worldName, dir, "", fetch.ListOptions{})
	if err != nil || listing.Response.Status != protocol.StatusOK {
		b.WriteString("\n(listing unavailable)\n")
		return
	}
	var lines []string
	for _, entry := range links.Extract(listing.Response.Body) {
		if entry == self {
			continue
		}
		lines = append(lines, "- "+entry)
	}
	fmt.Fprintf(b, " (%d)\n", len(lines))
	if len(lines) == 0 {
		b.WriteString("(none)\n")
		return
	}
	mdoutline.CappedList(b, lines, exploreSectionCap, "siblings")
}
