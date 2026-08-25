package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/listing"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// MCP resources: demarkus documents as client-attachable context. A
// resource read puts a document (or one #section of it) into the
// conversation without the model spending a tool turn — the Desktop
// picker / Claude Code @-mention path into demarkus.
//
// Resource reads deliberately bypass the mark_fetch ergonomics (no
// outline gate, no unchanged dedup): attaching is an explicit act by the
// user, and an outline or a notice where a document was requested would
// be the wrong surprise. #anchor URIs are the section-sized attach.

// maxListedResources caps how many documents the startup listing
// registers as concrete picker entries.
const maxListedResources = 50

// registerResources wires the resources surface: a URI template covering
// every document (with #anchor section support), plus the two well-known
// entry points every demarkus server has, so the picker is never empty.
func registerResources(s *mcpserver.MCPServer, h *handler, defaultHost string) {
	if defaultHost == "" {
		s.AddResourceTemplate(mcp.NewResourceTemplate(
			"mark://{host}/{+path}",
			"demarkus document",
			mcp.WithTemplateDescription("Any document on a Mark Protocol server, by host and path; append #<anchor> to attach a single section (anchors are GitHub-style heading slugs)."),
			mcp.WithTemplateMIMEType("text/markdown"),
		), h.readResource)
		return
	}

	s.AddResourceTemplate(mcp.NewResourceTemplate(
		defaultHost+"/{+path}",
		"demarkus document",
		mcp.WithTemplateDescription("Any document on "+defaultHost+", by path; append #<anchor> to attach a single section (anchors are GitHub-style heading slugs)."),
		mcp.WithTemplateMIMEType("text/markdown"),
	), h.readResource)

	s.AddResource(mcp.NewResource(
		defaultHost+"/index.md",
		"Index hub",
		mcp.WithResourceDescription("The server's index hub: the map of everything it holds."),
		mcp.WithMIMEType("text/markdown"),
	), h.readResource)
	s.AddResource(mcp.NewResource(
		defaultHost+protocol.WellKnownManifestPath,
		"Agent manifest",
		mcp.WithResourceDescription("The server's agent manifest: purpose, key paths, auth requirements, usage guidelines."),
		mcp.WithMIMEType("text/markdown"),
	), h.readResource)
}

// registerListedResources fetches the host's top-level listing and
// registers each document as a concrete resource so client pickers show
// real content, not just the two well-known entries. Best-effort by
// design: it runs in a goroutine after the server starts serving, and a
// down or slow host only costs the extra picker entries — never startup.
// Late additions notify connected clients via listChanged.
func registerListedResources(s *mcpserver.MCPServer, h *handler, defaultHost string) {
	host, path, err := h.resolveURL("/")
	if err != nil {
		// Unreachable in practice (callers gate on -host being set), but a
		// silent return would make any future regression undiagnosable.
		log.Printf("resource listing skipped (%v): picker shows well-known entries only", err)
		return
	}
	var resources []mcpserver.ServerResource
	cursor, lastName := "", ""
	seenCursors := make(map[string]struct{})
	for len(resources) < maxListedResources {
		result, err := h.client.ListWithOptions(host, path, h.resolveToken(host), fetch.ListOptions{
			Cursor:   cursor,
			PageSize: protocol.MaxListPageSize,
		})
		if err != nil || result.Response.Status != protocol.StatusOK {
			log.Printf("resource listing stopped (host %s unavailable): picker may be incomplete", host)
			break
		}
		page, err := listing.ParsePage(path, result.Response, lastName)
		if err != nil {
			log.Printf("resource listing stopped (host %s invalid response: %v): picker may be incomplete", host, err)
			break
		}
		for _, invalid := range page.Invalid {
			log.Printf("resource listing skipped invalid entry %q from host %s", invalid, host)
		}
		lastName = page.LastName
		for _, entry := range page.Entries {
			if len(resources) == maxListedResources {
				break
			}
			// Top-level documents only; directories and the already-registered
			// index stay out (re-registering index.md would clobber its entry).
			if entry.IsDir || !strings.HasSuffix(entry.Name, ".md") || entry.Name == "index.md" {
				continue
			}
			resources = append(resources, mcpserver.ServerResource{
				Resource: mcp.NewResource(
					defaultHost+"/"+url.PathEscape(entry.Name),
					entry.Name,
					mcp.WithResourceDescription("Document on "+defaultHost+"."),
					mcp.WithMIMEType("text/markdown"),
				),
				Handler: h.readResource,
			})
		}
		if page.Complete || len(resources) == maxListedResources {
			break
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate || page.NextCursor == cursor {
			log.Printf("resource listing stopped (host %s cursor did not advance): picker may be incomplete", host)
			break
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	if len(resources) == maxListedResources {
		log.Printf("resource listing capped at %d entries", maxListedResources)
	}
	if len(resources) > 0 {
		s.AddResources(resources...)
	}
}

// readResource serves every resource read: whole documents and #anchor
// sections, for both the concrete resources and the URI template.
func (h *handler) readResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	raw := req.Params.URI
	docURL, anchor, _ := strings.Cut(raw, "#")

	host, path, err := h.resolveURL(docURL)
	if err != nil {
		return nil, fmt.Errorf("invalid resource URI %q: %w", raw, err)
	}
	result, err := h.client.Fetch(host, path, h.resolveToken(host))
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
