package marktools

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/listing"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
)

// exploreSectionCap bounds each section of the card; overflow reads "+N more".
const exploreSectionCap = 10

// ExploreArgs are mark_explore's arguments. Relations switches the graph
// section from backlinks to a page of the document's neighborhood.
type ExploreArgs struct {
	URL       string
	Render    mcpfmt.Options
	Relations *RelationsArgs
}

// RelationsArgs page through a document's relations.
type RelationsArgs struct {
	Options graphstore.NeighborhoodOptions
	// Revalidate refreshes the linking sources first; incoming relations need it.
	Revalidate bool
}

// Explore answers mark_explore: one card that orients an agent on a document
// without its body. Only the fetch can fail it; every other part degrades.
func (t *Tools) Explore(ctx context.Context, args ExploreArgs) Result { //nolint:gocritic // arguments by value, like every tool
	docURL, _, _ := strings.Cut(args.URL, "#")
	target, bad := t.resolve(ctx, docURL)
	if bad != nil {
		return *bad
	}
	doc := fetch.FetchRequest{Host: target.Host, Path: target.Path, Token: t.readToken(ctx, target.Host)}
	result, err := t.backend.Fetch(ctx, doc)
	if err != nil {
		return t.failed(SiteExplore, target.Host, err)
	}
	body := result.Response.Body
	binary := mdoutline.BinaryBody(body)
	observed := result
	if binary {
		observed.Response.Body = ""
	}
	// No store is not a failure here: the card says so in its graph section.
	scope, _ := t.graphScope(ctx)
	if result.Response.Status != protocol.StatusOK {
		out := mcpfmt.Format(result, args.Render)
		if warning := observe(scope, &target, observed); warning != "" {
			out += mcpfmt.Note(strings.TrimSuffix(warning, "\n"))
		}
		return text(out)
	}
	if scope != nil {
		scope.seed(ctx, target)
	}
	cacheWarning := observe(scope, &target, observed)

	// An outline over a binary body is garbage; say what it is instead.
	if binary {
		notice := mdoutline.NonMarkdownNotice(len(body))
		if cacheWarning != "" {
			notice += "\n" + cacheWarning
		}
		return text(mcpfmt.FormatWith(result, notice, map[string]string{"mode": "binary"}, args.Render))
	}

	var b strings.Builder
	writeDocumentSections(&b, body)
	b.WriteByte('\n')
	b.WriteString(cacheWarning)
	if bad := t.writeGraphSection(ctx, &b, graphSection{scope: scope, target: target, relations: args.Relations}); bad != nil {
		return *bad
	}
	t.writeSiblingsSection(ctx, &b, doc)
	fmt.Fprintf(&b, "\nfetch %s#<anchor> for a section; mark_fetch force=true for the full body\n", docURL)

	extra := map[string]string{"size": fmt.Sprintf("%d bytes, %d lines", len(body), strings.Count(body, "\n")+1)}
	return text(mcpfmt.FormatWith(result, b.String(), extra, args.Render))
}

// observe records what the fetch saw in the graph store and saves it; the
// string is a warning for the card when the save failed.
func observe(scope *GraphScope, target *Target, result fetch.Result) string {
	if scope == nil {
		return ""
	}
	source := ""
	if scope.Source != nil {
		source = scope.Source(*target)
	}
	store := scope.Store
	if !store.ObserveDocument(target.NodeURL, graph.FetchResult{
		Source: source, Status: result.Response.Status, Body: result.Response.Body, Metadata: result.Response.Metadata,
	}) {
		return ""
	}
	if err := store.Save(); err != nil {
		return fmt.Sprintf("graph cache save failed: %v\n", err)
	}
	return ""
}

// writeDocumentSections writes what the body alone tells: outline, opening, links.
func writeDocumentSections(b *strings.Builder, body string) {
	b.WriteString("## Outline\n")
	if tree := mdoutline.Outline(body); tree != "" {
		mdoutline.CappedList(b, strings.Split(strings.TrimRight(tree, "\n"), "\n"), exploreSectionCap, "headings")
	} else {
		b.WriteString("(no headings)\n")
	}
	if para := mdoutline.OpeningParagraph(body); para != "" {
		b.WriteString("\n## Opening\n")
		b.WriteString(para)
		b.WriteString("\n")
	}
	out := mdoutline.LinkLines(body)
	fmt.Fprintf(b, "\n## Outbound links (%d)\n", len(out))
	if len(out) == 0 {
		b.WriteString("(none)\n")
		return
	}
	mdoutline.CappedList(b, out, exploreSectionCap, "links")
}

// graphSection is what the card's graph part needs.
type graphSection struct {
	scope     *GraphScope
	target    Target
	relations *RelationsArgs
}

// writeGraphSection writes backlinks, or a page of relations when asked. A
// relations query that fails is the one graph problem that fails the card.
func (t *Tools) writeGraphSection(ctx context.Context, b *strings.Builder, g graphSection) *Result {
	if g.scope == nil {
		section := "Backlinks"
		if g.relations != nil {
			section = "Relations"
		}
		fmt.Fprintf(b, "## %s\n(graph store unavailable)\n", section)
		return nil
	}
	if g.relations == nil {
		b.WriteString(mcpfmt.FormatExploreBacklinks(g.scope.Store.BacklinksEnriched(g.target.NodeURL)))
		return nil
	}
	if g.relations.Revalidate {
		b.WriteString(g.scope.Store.RevalidateBacklinks(ctx, g.target.NodeURL, g.scope.Fetch))
	}
	page, err := g.scope.Store.Neighborhood(g.target.NodeURL, g.relations.Options)
	if err != nil {
		bad := failure("query relations: %v", err)
		return &bad
	}
	b.WriteString(mcpfmt.FormatNeighborhood(g.target.NodeURL, page))
	return nil
}

// writeSiblingsSection lists the document's directory. A failed LIST degrades
// to a note, never an error.
func (t *Tools) writeSiblingsSection(ctx context.Context, b *strings.Builder, doc fetch.FetchRequest) {
	dir := doc.Path[:strings.LastIndex(doc.Path, "/")+1]
	self := doc.Path[strings.LastIndex(doc.Path, "/")+1:]

	fmt.Fprintf(b, "\n## Siblings in %s", dir)
	// Two past the cap: the document itself is one entry, and one more is what
	// tells "exactly the cap" from "more than the cap".
	result, err := t.backend.List(ctx, fetch.ListRequest{Host: doc.Host, Path: dir, Token: doc.Token, PageSize: exploreSectionCap + 2})
	if err != nil || result.Response.Status != protocol.StatusOK {
		b.WriteString("\n(listing unavailable)\n")
		return
	}
	page, err := listing.ParsePage(dir, result.Response, "")
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
		if name != self {
			lines = append(lines, "- "+name)
		}
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
