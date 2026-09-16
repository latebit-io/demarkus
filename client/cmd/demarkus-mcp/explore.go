package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/links"
	listformat "github.com/latebit-io/demarkus/client/listing"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// exploreSectionCap bounds each section of the neighborhood card. Overflow
// is reported honestly as "+N more".
const exploreSectionCap = 10

func markExploreTool(host string) mcp.Tool {
	neighborhoodParams := mcpfmt.NeighborhoodParams()
	options := make([]mcp.ToolOption, 0, len(neighborhoodParams)+3)
	options = append(options,
		mcp.WithDescription(mcpfmt.ExploreDescription+urlHint(host)),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
	)
	options = append(options, neighborhoodParams...)
	options = append(options, mcpfmt.Fetch.Param())
	return mcp.NewTool("mark_explore", options...)
}

func (h *handler) markExplore(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	docURL, _, _ := strings.Cut(rawURL, "#")
	opts := mcpfmt.Fetch.Options(&req)
	neighborhoodOpts := mcpfmt.NeighborhoodOptions(&req)
	neighborhoodRequested := mcpfmt.NeighborhoodRequested(&req)

	host, path, err := h.resolveURL(docURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	token := h.resolveToken(host)
	result, err := h.client.Fetch(host, path, token)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch failed: %v", err)), nil
	}
	body := result.Response.Body
	binary := mdoutline.BinaryBody(body)
	observed := result
	if binary {
		observed.Response.Body = ""
	}
	fullURL := links.NodeURL(host, path)
	if result.Response.Status != protocol.StatusOK {
		text := mcpfmt.Format(result, opts)
		if warning := h.observeExploreDocument(fullURL, observed); warning != "" {
			text += mcpfmt.Note(strings.TrimSuffix(warning, "\n"))
		}
		return mcp.NewToolResultText(text), nil
	}
	h.seedGraph(ctx, host)
	cacheWarning := h.observeExploreDocument(fullURL, observed)

	// Binary/non-UTF-8 body: an outline over it is garbage; return a notice.
	if binary {
		notice := mdoutline.NonMarkdownNotice(len(body))
		if cacheWarning != "" {
			notice += "\n" + cacheWarning
		}
		return mcp.NewToolResultText(mcpfmt.FormatWith(result, notice,
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
	if cacheWarning != "" {
		b.WriteString(cacheWarning)
	}
	if h.graphStore == nil {
		section := "Backlinks"
		if neighborhoodRequested {
			section = "Relations"
		}
		fmt.Fprintf(&b, "## %s\n(graph store unavailable)\n", section)
	} else {
		if !neighborhoodRequested || neighborhoodOpts.Direction != graphstore.NeighborhoodOutgoing {
			b.WriteString(h.revalidateBacklinks(ctx, fullURL))
		}
		if neighborhoodRequested {
			page, queryErr := h.graphStore.Neighborhood(fullURL, neighborhoodOpts)
			if queryErr != nil {
				return mcp.NewToolResultError(fmt.Sprintf("query relations: %v", queryErr)), nil
			}
			b.WriteString(mcpfmt.FormatNeighborhood(fullURL, page))
		} else {
			b.WriteString(mcpfmt.FormatExploreBacklinks(h.graphStore.BacklinksEnriched(fullURL)))
		}
	}
	h.writeSiblingsSection(&b, host, path, token)

	fmt.Fprintf(&b, "\nfetch %s#<anchor> for a section; mark_fetch force=true for the full body\n", docURL)

	extra := map[string]string{
		"size": fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1),
	}
	return mcp.NewToolResultText(mcpfmt.FormatWith(result, b.String(), extra, opts)), nil
}

func (h *handler) observeExploreDocument(fullURL string, result fetch.Result) string {
	if h.graphStore == nil || !h.graphStore.ObserveDocument(fullURL, graph.FetchResult{
		Status: result.Response.Status, Body: result.Response.Body, Metadata: result.Response.Metadata,
	}) {
		return ""
	}
	if err := h.graphStore.Save(); err != nil {
		return fmt.Sprintf("graph cache save failed: %v\n", err)
	}
	return ""
}

// writeSiblingsSection appends the sibling listing of the document's parent
// directory. A failed LIST degrades to a note, never an error.
func (h *handler) writeSiblingsSection(b *strings.Builder, host, path, token string) {
	dir := path[:strings.LastIndex(path, "/")+1]
	self := path[strings.LastIndex(path, "/")+1:]

	fmt.Fprintf(b, "\n## Siblings in %s", dir)
	result, err := h.client.ListWithOptions(host, dir, token, fetch.ListOptions{PageSize: exploreSectionCap + 1})
	if err != nil || result.Response.Status != protocol.StatusOK {
		b.WriteString("\n(listing unavailable)\n")
		return
	}
	page, err := listformat.ParsePage(dir, result.Response, "")
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
		b.WriteString(" (first page)\n")
	}
	if len(lines) == 0 {
		b.WriteString("(none)\n")
		return
	}
	mdoutline.CappedList(b, lines, exploreSectionCap, "siblings")
}
