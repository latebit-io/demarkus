package broker

import (
	"context"
	"errors"
	"fmt"

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

// registerResources wires a URI template for every document, plus each
// world's index hub so the picker shows real entry points. Only the knowledge
// profile lists worlds, and its set is static per pod: once is complete.
func (g *mcpGateway) registerResources() {
	if g.profile.TenantScoped {
		// Per-world hub resources would leak every tenant's world name
		// into every session's picker; the template plus the
		// readResource tenant gate is the whole surface.
		g.mcpServer.AddResourceTemplate(mcp.NewResourceTemplate(
			"mark://{world}/{+path}",
			"memory document",
			mcp.WithTemplateDescription("Any document in your memory, by world name and path; append #<anchor> to attach a single section (anchors are GitHub-style heading slugs). Your world name comes from the mark_worlds tool."),
			mcp.WithTemplateMIMEType("text/markdown"),
		), g.readResource)
		return
	}
	g.mcpServer.AddResourceTemplate(mcp.NewResourceTemplate(
		"mark://{world}/{+path}",
		"knowledge-system document",
		mcp.WithTemplateDescription("Any document in this knowledge system, by world name and path; append #<anchor> to attach a single section (anchors are GitHub-style heading slugs). List worlds with the mark_worlds tool."),
		mcp.WithTemplateMIMEType("text/markdown"),
	), g.readResource)

	worlds := readableWorlds(g.deps.Worlds)
	for j := range worlds {
		w := &worlds[j]
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
func (g *mcpGateway) readResource(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	raw := req.Params.URI
	ctx, err := g.admitResourceRead(ctx, raw)
	if err != nil {
		return nil, err
	}
	if g.tools == nil {
		return nil, errors.New("internal: tool bodies unavailable")
	}
	resource, err := g.tools.ReadResource(ctx, raw)
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{mcp.TextResourceContents{URI: raw, MIMEType: resource.MIMEType, Text: resource.Text}}, nil
}

// admitResourceRead is tenantGate for resource reads, which bypass the tool
// middleware: the same door, then the same one world rule.
func (g *mcpGateway) admitResourceRead(ctx context.Context, raw string) (context.Context, error) {
	if !g.profile.TenantScoped {
		return ctx, nil
	}
	ctx, w, err := g.admitTenant(ctx)
	if err != nil {
		return ctx, err
	}
	owns, parseErr := tenantOwns(&w, raw)
	if parseErr != nil {
		return ctx, fmt.Errorf("invalid resource URI %q: %w", raw, parseErr)
	}
	if !owns {
		g.deps.Log.Warn("resource read denied cross-tenant access", "world", w.Name)
		return ctx, crossTenantDenial(&w)
	}
	g.seedTenant(ctx, &w)
	return ctx, nil
}
