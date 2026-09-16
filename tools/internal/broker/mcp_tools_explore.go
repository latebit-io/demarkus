package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	listingpage "github.com/latebit-io/demarkus/client/listing"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// exploreSectionCap bounds each section of the neighborhood card.
// Overflow is reported honestly as "+N more". Matches the local
// demarkus-mcp value.
const exploreSectionCap = 10

// Explore mirrors the client card, using world-name URLs and scoped backlinks.
func (g *mcpGateway) handleMarkExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go's AddTool API
	raw, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	docURL, _, _ := strings.Cut(raw, "#")
	opts := mcpfmt.Fetch.Options(&req)
	neighborhoodOpts := mcpfmt.NeighborhoodOptions(&req)
	neighborhoodRequested := mcpfmt.NeighborhoodRequested(&req)

	worldName, path, err := parseToolURL(docURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}
	result, err := g.dispatcher.Fetch(worldName, path, "")
	if err != nil {
		return g.toolErrorFor("explore", worldName, err), nil
	}
	body := result.Response.Body
	binary := mdoutline.BinaryBody(body)
	observed := result
	if binary {
		observed.Response.Body = ""
	}
	state, err := g.graphFor(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("graph scope: %v", err)), nil
	}
	g.seedGraphStore(ctx, state)
	source := docURL
	if world, ok := g.srv.cfg.FindWorld(worldName); ok {
		source = links.NodeURL(resolveWorldAddress(&world), path)
	}
	state.graphStore.ObserveDocument(docURL, graph.FetchResult{
		Source: source, Status: observed.Response.Status, Body: observed.Response.Body, Metadata: observed.Response.Metadata,
	})
	if result.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultText(mcpfmt.Format(result, opts)), nil
	}

	// Binary/non-UTF-8 body: an outline over it is garbage; return a notice.
	if binary {
		return mcp.NewToolResultText(mcpfmt.FormatWith(result, mdoutline.NonMarkdownNotice(len(body)),
			map[string]string{"mode": "binary"}, opts)), nil
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

	b.WriteByte('\n')
	if mcpfmt.RevalidatesBacklinks(&req) {
		b.WriteString(g.revalidateBacklinks(ctx, state, docURL))
	}
	if neighborhoodRequested {
		page, queryErr := state.graphStore.Neighborhood(docURL, neighborhoodOpts)
		if queryErr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("query relations: %v", queryErr)), nil
		}
		b.WriteString(mcpfmt.FormatNeighborhood(docURL, page))
	} else {
		b.WriteString(mcpfmt.FormatExploreBacklinks(state.graphStore.BacklinksEnriched(docURL)))
	}
	g.writeSiblingsSection(&b, worldName, path)

	fmt.Fprintf(&b, "\nfetch %s#<anchor> for a section; mark_fetch force=true for the full body\n", docURL)

	extra := map[string]string{
		"size": fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1),
	}
	return mcp.NewToolResultText(mcpfmt.FormatWith(result, b.String(), extra, opts)), nil
}

// writeSiblingsSection appends the sibling listing of the document's
// parent directory. A failed LIST degrades to a note, never an error.
func (g *mcpGateway) writeSiblingsSection(b *strings.Builder, worldName, path string) {
	dir := path[:strings.LastIndex(path, "/")+1]
	self := path[strings.LastIndex(path, "/")+1:]

	fmt.Fprintf(b, "\n## Siblings in %s", dir)
	result, err := g.dispatcher.List(worldName, dir, "", fetch.ListOptions{PageSize: exploreSectionCap + 2})
	if err != nil || result.Response.Status != protocol.StatusOK {
		b.WriteString("\n(listing unavailable)\n")
		return
	}
	page, err := listingpage.ParsePage(dir, result.Response, "")
	if err != nil || len(page.Invalid) > 0 {
		b.WriteString("\n(listing unavailable)\n")
		return
	}
	var lines []string
	for _, entry := range page.Entries {
		name := entry.Name
		if entry.IsDir {
			name += "/"
		}
		if name == self {
			continue
		}
		lines = append(lines, "- "+name)
	}
	if page.Complete {
		fmt.Fprintf(b, " (%d)\n", len(lines))
	} else {
		fmt.Fprintf(b, " (%d+)\n", len(lines))
	}
	if len(lines) == 0 {
		b.WriteString("(none)\n")
		return
	}
	mdoutline.CappedList(b, lines, exploreSectionCap, "siblings")
}
