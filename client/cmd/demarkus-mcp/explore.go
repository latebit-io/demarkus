package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// exploreSectionCap bounds each section of the neighborhood card. Overflow
// is reported honestly as "+N more".
const exploreSectionCap = 10

func markExploreTool(host string) mcp.Tool {
	return mcp.NewTool("mark_explore",
		mcp.WithDescription(
			"Orient around one document in a single call: its outline head (heading "+
				"tree with #anchors plus the opening paragraph), outbound links, recorded "+
				"backlinks, and sibling documents in the same directory — each section "+
				"capped at 10 entries. Use this instead of a fetch + backlinks + list "+
				"chain when you need to understand what a document covers and what to "+
				"read next; then mark_fetch url#<anchor> for the sections that matter. "+
				"Backlinks come from the local graph store (mark_graph populates it). "+
				urlHint(host),
		),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description(urlDesc(host)),
		),
	)
}

func (h *handler) markExplore(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) { //nolint:gocritic // signature required by mcp-go
	rawURL, err := req.RequireString("url")
	if err != nil {
		return mcp.NewToolResultError("url is required"), nil
	}
	docURL, _, _ := strings.Cut(rawURL, "#")

	host, path, err := h.resolveURL(docURL)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("invalid URL: %v", err)), nil
	}

	token := h.resolveToken(host)
	result, err := h.client.Fetch(host, path, token)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("fetch failed: %v", err)), nil
	}
	if result.Response.Status != protocol.StatusOK {
		return mcp.NewToolResultText(formatResult(result, "version", "modified", "etag")), nil
	}
	body := result.Response.Body

	var b strings.Builder

	b.WriteString("## Outline\n")
	if tree := mdoutline.Outline(body); tree != "" {
		writeCapped(&b, strings.Split(strings.TrimRight(tree, "\n"), "\n"), "headings")
	} else {
		b.WriteString("(no headings)\n")
	}
	if para := mdoutline.OpeningParagraph(body); para != "" {
		b.WriteString("\n## Opening\n")
		b.WriteString(para)
		b.WriteString("\n")
	}

	out := outboundLinkLines(body)
	fmt.Fprintf(&b, "\n## Outbound links (%d)\n", len(out))
	if len(out) == 0 {
		b.WriteString("(none)\n")
	} else {
		writeCapped(&b, out, "links")
	}

	h.writeBacklinksSection(&b, docURL)
	h.writeSiblingsSection(&b, host, path, token)

	fmt.Fprintf(&b, "\nfetch %s#<anchor> for a section; mark_fetch force=true for the full body\n", docURL)

	extra := map[string]string{
		"size": fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1),
	}
	return mcp.NewToolResultText(formatResultWith(result, b.String(), extra, "version", "modified", "etag")), nil
}

// writeBacklinksSection appends the backlinks card section from the local
// graph store. An empty or missing store degrades to a note, never an error.
func (h *handler) writeBacklinksSection(b *strings.Builder, docURL string) {
	fullURL := docURL
	if strings.HasPrefix(docURL, "/") {
		fullURL = h.defaultHost + docURL
	}
	if h.graphStore == nil {
		b.WriteString("\n## Backlinks\n(graph store unavailable)\n")
		return
	}
	backlinks := h.graphStore.BacklinksEnriched(fullURL)
	fmt.Fprintf(b, "\n## Backlinks (%d)\n", len(backlinks))
	if len(backlinks) == 0 {
		b.WriteString("(none recorded — run mark_graph to populate)\n")
		return
	}
	lines := make([]string, len(backlinks))
	for i, bl := range backlinks {
		if bl.Title != "" {
			lines[i] = fmt.Sprintf("- [%s](%s)", bl.Title, bl.URL)
		} else {
			lines[i] = "- " + bl.URL
		}
	}
	writeCapped(b, lines, "backlinks")
}

// writeSiblingsSection appends the sibling listing of the document's parent
// directory. A failed LIST degrades to a note, never an error.
func (h *handler) writeSiblingsSection(b *strings.Builder, host, path, token string) {
	dir := path[:strings.LastIndex(path, "/")+1]
	self := path[strings.LastIndex(path, "/")+1:]

	fmt.Fprintf(b, "\n## Siblings in %s", dir)
	listing, err := h.client.List(host, dir, token)
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
	writeCapped(b, lines, "siblings")
}

// outboundLinkLines extracts the document's links as one-line entries with
// titles, deduplicated by destination in document order.
func outboundLinkLines(body string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, l := range links.ExtractWithPositions(body) {
		if seen[l.Dest] {
			continue
		}
		seen[l.Dest] = true
		if l.Text != "" {
			out = append(out, fmt.Sprintf("- [%s](%s)", l.Text, l.Dest))
		} else {
			out = append(out, "- "+l.Dest)
		}
	}
	return out
}

// writeCapped writes at most exploreSectionCap pre-formatted lines and an
// honest "+N more <noun>" marker for the overflow.
func writeCapped(b *strings.Builder, lines []string, noun string) {
	for i, line := range lines {
		if i == exploreSectionCap {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if n := len(lines) - exploreSectionCap; n > 0 {
		fmt.Fprintf(b, "+%d more %s\n", n, noun)
	}
}
