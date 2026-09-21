package marktools

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
)

// SeenStore remembers, for one agent, which version of a document was last
// returned in full. A surface scopes it: the process, or an MCP session.
type SeenStore interface {
	Lookup(ctx context.Context, key string) (fetchdedup.Doc, bool)
	Record(ctx context.Context, key string, d fetchdedup.Doc)
}

// FetchArgs are mark_fetch's arguments. URL may carry a #anchor.
type FetchArgs struct {
	URL    string
	Force  bool
	Render mcpfmt.Options
}

// Fetch answers mark_fetch: a section, an unchanged notice, an outline of a
// large document, or the full body, in that order of precedence.
func (t *Tools) Fetch(ctx context.Context, args FetchArgs) Result {
	docURL, anchor, _ := strings.Cut(args.URL, "#")
	target, bad := t.resolve(ctx, docURL)
	if bad != nil {
		return *bad
	}
	result, err := t.backend.Fetch(ctx, fetch.FetchRequest{
		Host: target.Host, Path: target.Path, Token: t.readToken(ctx, target.Host),
	})
	if err != nil {
		return t.failed(SiteFetch, target.Host, err)
	}
	if result.Response.Status != protocol.StatusOK {
		return text(mcpfmt.Format(result, args.Render))
	}
	body := result.Response.Body

	// MCP text cannot carry binary faithfully; byte exact retrieval is the CLI's job.
	if mdoutline.BinaryBody(body) {
		return text(mcpfmt.FormatWith(result, mdoutline.NonMarkdownNotice(len(body)), map[string]string{"mode": "binary"}, args.Render))
	}
	// A section works at any size and skips dedup: it asks for content the
	// agent has not necessarily seen.
	if anchor != "" {
		section, err := sectionOf(body, docURL, anchor)
		if err != nil {
			return failure("%v", err)
		}
		return text(mcpfmt.FormatWith(result, section, map[string]string{"section": "#" + anchor}, args.Render))
	}
	return t.fullOrOutline(ctx, &fetched{target: target, docURL: docURL, result: result}, args)
}

// fetched is an ok, textual FETCH answer on its way to the agent.
type fetched struct {
	target Target
	docURL string
	result fetch.Result
}

func (t *Tools) fullOrOutline(ctx context.Context, f *fetched, args FetchArgs) Result {
	meta, body := f.result.Response.Metadata, f.result.Response.Body
	// Dedup needs an identity: with version and etag both absent, a changed
	// document would compare equal and be reported as unchanged.
	cur := fetchdedup.Doc{Version: meta["version"], Etag: meta["etag"]}
	key := f.target.Host + f.target.Path
	prev, seenBefore := t.seenLookup(ctx, key)
	if seenBefore && !args.Force && cur.Identified() && prev == cur {
		return text(fetchdedup.UnchangedNotice(cur, args.Render.Verbose))
	}
	extra := map[string]string{}
	if seenBefore && cur.Identified() && prev != cur {
		extra["note"] = fetchdedup.ChangedNote(prev, cur)
	}
	// Size gate: a large document answers with its outline unless forced.
	if !args.Force && len(body) >= mdoutline.OutlineThreshold {
		extra["mode"] = "outline"
		extra["size"] = fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1)
		return text(mcpfmt.FormatWith(f.result, mdoutline.OutlineBody(f.docURL, body), extra, args.Render))
	}
	if cur.Identified() && t.hooks.Seen != nil {
		t.hooks.Seen.Record(ctx, key, cur)
	}
	return text(mcpfmt.FormatWith(f.result, body, extra, args.Render))
}

func (t *Tools) seenLookup(ctx context.Context, key string) (fetchdedup.Doc, bool) {
	if t.hooks.Seen == nil {
		return fetchdedup.Doc{}, false
	}
	return t.hooks.Seen.Lookup(ctx, key)
}

// sectionOf slices one section, or names the anchors that do exist.
func sectionOf(body, docURL, anchor string) (string, error) {
	section, ok := mdoutline.Section(body, anchor)
	if ok {
		return section, nil
	}
	available := strings.Join(mdoutline.Anchors(body), ", ")
	if available == "" {
		available = "(document has no headings)"
	}
	return "", fmt.Errorf("section #%s not found in %s; available anchors: %s", anchor, docURL, available)
}

// Resource is one MCP resource read.
type Resource struct {
	MIMEType, Text string
}

// ReadResource answers a resource read: the document, or the #anchor section.
func (t *Tools) ReadResource(ctx context.Context, rawURI string) (Resource, error) {
	docURL, anchor, _ := strings.Cut(rawURI, "#")
	target, err := t.hooks.Resolve(ctx, docURL)
	if err != nil {
		return Resource{}, fmt.Errorf("invalid resource URI %q: %w", rawURI, err)
	}
	result, err := t.backend.Fetch(ctx, fetch.FetchRequest{
		Host: target.Host, Path: target.Path, Token: t.readToken(ctx, target.Host),
	})
	if err != nil {
		return Resource{}, fmt.Errorf("fetch %s: %w", docURL, err)
	}
	if result.Response.Status != protocol.StatusOK {
		return Resource{}, fmt.Errorf("%s: %s", docURL, result.Response.Status)
	}
	body := result.Response.Body
	// A binary body reads as a plain text notice, never as mojibake.
	if mdoutline.BinaryBody(body) {
		return Resource{MIMEType: "text/plain", Text: mdoutline.NonMarkdownNotice(len(body))}, nil
	}
	if anchor != "" {
		if body, err = sectionOf(body, docURL, anchor); err != nil {
			return Resource{}, err
		}
	}
	return Resource{MIMEType: "text/markdown", Text: body}, nil
}
