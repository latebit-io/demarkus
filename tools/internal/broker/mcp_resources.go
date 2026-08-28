package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCP resources on the gateway: knowledge-system documents as
// client-attachable context (Desktop picker, Claude Code @-mentions),
// zero tool turns. Mirrors client/cmd/demarkus-mcp's resources surface
// with the broker's world-name addressing.
//
// Auth: the /mcp transport sits behind gatewayAuth, so every resource
// read is already an SSO-authenticated caller; reads then dispatch with
// an empty bearer exactly like handleMarkFetch (reads are open to any
// authenticated identity — the org gate is the access control).
//
// Resource reads deliberately bypass the mark_fetch ergonomics (no
// outline gate, no session dedup): attaching is an explicit act, and an
// outline or an "unchanged" notice where a document was requested would
// be the wrong surprise. #anchor URIs are the section-sized attach.

// registerResources wires the resources surface: a URI template covering
// every document in every world, plus each configured world's index hub
// so the picker shows real entry points. The world set is config-static
// per pod (worlds[] changes roll the broker), so construction-time
// registration is complete — no dynamic listing needed.
func (g *mcpGateway) registerResources() {
	g.mcpServer.AddResourceTemplate(mcp.NewResourceTemplate(
		"mark://{world}/{+path}",
		"knowledge-system document",
		mcp.WithTemplateDescription("Any document in this knowledge system, by world name and path; append #<anchor> to attach a single section (anchors are GitHub-style heading slugs). List worlds with the mark_worlds tool."),
		mcp.WithTemplateMIMEType("text/markdown"),
	), g.readResource)

	for _, w := range readableWorlds(g.srv.cfg) {
		g.mcpServer.AddResource(mcp.NewResource(
			fmt.Sprintf("mark://%s/index.md", w.Name),
			w.Name+": index hub",
			mcp.WithResourceDescription(fmt.Sprintf("The %s world's index hub: the map of everything it holds.", w.Name)),
			mcp.WithMIMEType("text/markdown"),
		), g.readResource)
	}
}

// readResource serves every resource read: whole documents and #anchor
// sections, for both the concrete per-world hubs and the URI template.
func (g *mcpGateway) readResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	raw := req.Params.URI
	// The fragment is the section selector, cut before URL validation —
	// parseToolURL rejects fragments because elsewhere they would be
	// silently dropped; here the fragment has defined meaning.
	docURL, anchor, _ := strings.Cut(raw, "#")

	worldName, path, err := parseToolURL(docURL)
	if err != nil {
		return nil, fmt.Errorf("invalid resource URI %q: %w", raw, err)
	}
	result, err := g.dispatcher.Fetch(worldName, path, "")
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", docURL, err)
	}
	if result.Response.Status != protocol.StatusOK {
		return nil, fmt.Errorf("%s: %s", docURL, result.Response.Status)
	}

	body := result.Response.Body
	// Binary/non-UTF-8 body: return a plain-text notice, not mojibake.
	if mdoutline.BinaryBody(body) {
		return []mcp.ResourceContents{mcp.TextResourceContents{
			URI:      raw,
			MIMEType: "text/plain",
			Text:     mdoutline.NonMarkdownNotice(len(body)),
		}}, nil
	}
	if anchor != "" {
		section, ok := mdoutline.Section(body, anchor)
		if !ok {
			available := strings.Join(mdoutline.Anchors(body), ", ")
			if available == "" {
				available = "(document has no headings)"
			}
			return nil, fmt.Errorf("section #%s not found in %s; available anchors: %s", anchor, docURL, available)
		}
		body = section
	}

	return []mcp.ResourceContents{mcp.TextResourceContents{
		URI:      raw,
		MIMEType: "text/markdown",
		Text:     body,
	}}, nil
}
