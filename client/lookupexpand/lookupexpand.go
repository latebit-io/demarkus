// Package lookupexpand appends the matched sections' text to a LOOKUP table
// within a byte budget, so one lookup call can answer a task on both MCP
// surfaces without a fetch per row.
package lookupexpand

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/lookuptable"
	"github.com/latebit-io/demarkus/client/mdoutline"
)

// Param is the tool argument: approximate result tokens for the whole result.
const Param = "budget"

// ParamDesc describes the budget argument on both surfaces.
const ParamDesc = "result tokens for expansion: matched sections follow the table in rank order under '" + Delimiter + " path#anchor' lines, whole sections, until spent (default 0: table only)"

// Delimiter opens every expanded block and every note. Body lines that start
// with it (a nested blockquote) are indented one space, so only the frame
// carries it at column zero.
const Delimiter = ">>>"

// bytesPerToken is the budget's unit; the surfaces carry no tokenizer.
const bytesPerToken = 4

// MaxFetches bounds the documents one expansion fetches, direct and related:
// the budget bounds output, this bounds wire calls.
const MaxFetches = 10

// MaxRelated bounds the related documents one expansion adds.
const MaxRelated = 3

// maxUnfitListed bounds the rows the did-not-fit note names.
const maxUnfitListed = 3

// PreferredRelations are the rel-<predicate> keys followed first when the
// strongest match declares more relations than MaxRelated: supersession,
// dependency, implementation and derivation carry evidence, not navigation.
var PreferredRelations = []string{"supersedes", "superseded-by", "depends-on", "implements", "derived-from"}

// Document is what a Fetch returns: the body, the immutable version it came
// from when the source reports one (so a block can be cited as path/vN#anchor
// rather than as a head that may have moved on), and the document's metadata.
type Document struct {
	Body     string
	Version  int
	Metadata map[string]string
}

// Fetch returns the document at a table row's location (a bare path or a
// mark:// URL, as the table printed it).
type Fetch func(ctx context.Context, location string) (Document, error)

// Input is one expansion request. Budget is bytes for the whole result;
// Spent is what the surface printed before the expansion (envelope, table,
// notes), so the expansion gets the remainder.
type Input struct {
	Table  string
	Query  string
	Budget int
	Spent  int
	Fetch  Fetch
}

// pinned is the location with its version, when known, ahead of the anchor.
func pinned(path, anchor string, version int) string {
	loc := path
	if version > 0 {
		loc += "/v" + strconv.Itoa(version)
	}
	if anchor != "" {
		loc += "#" + anchor
	}
	return loc
}

// Option declares the budget argument on a tool.
func Option() mcp.ToolOption {
	return mcp.WithNumber(Param, mcp.Description(ParamDesc))
}

// Budget reads the argument as bytes; zero when absent or not positive.
func Budget(req *mcp.CallToolRequest) int {
	if n := req.GetInt(Param, 0); n > 0 {
		return n * bytesPerToken
	}
	return 0
}

// span is a printed byte range of one document and the block that holds it.
type span struct {
	start, end int
	loc        string
}

// cached is a fetched document with its headings parsed once per path; ok is
// false when the fetch failed, so an empty document is not mistaken for one.
type cached struct {
	Document
	headings []mdoutline.Heading
	ok       bool
}

// piece is one candidate block: a section or whole document of path.
type piece struct {
	path, anchor string
	version      int
	start, end   int // byte range in the body; equal when the text is not a range (an outline)
	text         string
}

// expansion accumulates blocks against the remaining budget.
type expansion struct {
	b          strings.Builder
	remaining  int
	expanded   int
	related    int
	fetches    int
	limitNoted bool
	terms      []string
	fetch      Fetch
	docs       map[string]*cached // fetched by path
	spans      map[string][]span  // printed ranges per path
	texts      map[string]string  // printed text to its first location
	linked     map[string]bool    // paths whose relation line was printed
	unfit      []string
	unfitMore  int
}

// write appends text, charging it to the budget; notes may overdraw.
func (e *expansion) write(s string) {
	e.b.WriteString(s)
	e.remaining -= len(s)
}

// add appends a block when it fits whole and reports whether it did. The
// size check precedes the copy; escaping only grows text, so a reject is safe.
func (e *expansion) add(loc, text string) bool {
	const frame = len(Delimiter) + 1 + 2 + 2
	if frame+len(loc)+len(text) > e.remaining {
		return false
	}
	block := Delimiter + " " + loc + "\n\n" + escapeFrame(text) + "\n\n"
	if len(block) > e.remaining {
		return false
	}
	e.write(block)
	return true
}

// note appends a delimited note line.
func (e *expansion) note(format string, args ...any) {
	before := e.b.Len()
	fmt.Fprintf(&e.b, Delimiter+" note: "+format+"\n", args...)
	e.remaining -= e.b.Len() - before
}

// row is one table row's location, split once.
type row struct{ path, anchor string }

// Expand renders the rows' sections in rank order, whole sections only, then
// the strongest match's related documents, until the budget or MaxFetches is
// spent or ctx ends; a large document's bare row expands to the term-matching sections, else its outline.
func Expand(ctx context.Context, in Input) string {
	rows := locations(in.Table)
	if len(rows) == 0 || in.Budget <= 0 {
		return ""
	}
	e := &expansion{
		remaining: in.Budget - in.Spent, terms: queryTerms(in.Query), fetch: in.Fetch,
		docs: make(map[string]*cached), spans: make(map[string][]span),
		texts: make(map[string]string), linked: make(map[string]bool),
	}
	if e.remaining <= 0 {
		e.note("the table alone used %d of %d result tokens; raise budget or fetch a row", in.Spent/bytesPerToken, in.Budget/bytesPerToken)
		return "\n" + e.b.String()
	}
	inTable := make(map[string]bool, len(rows))
	for _, r := range rows {
		inTable[r.path] = true
	}
	// Only the first document printed, the strongest match, seeds the hop.
	var top string
	var candidates []graph.RelRef
	for _, r := range rows {
		if e.remaining <= 0 || e.stopped(ctx) {
			break
		}
		doc, ok := e.document(ctx, r.path)
		if !ok || e.render(r.path, r.anchor, doc) == 0 {
			continue
		}
		e.expanded++
		refs := e.relations(r.path, doc)
		if top == "" {
			top, candidates = r.path, refs
		}
	}
	e.expandRelated(ctx, top, preferFirst(candidates), inTable)
	e.unfitNote()
	if e.related > 0 {
		e.note("expanded %d of %d rows and %d related documents within the budget", e.expanded, len(rows), e.related)
	} else {
		e.note("expanded %d of %d rows within the budget", e.expanded, len(rows))
	}
	return "\n" + e.b.String()
}

// stopped reports a finished context, once, as a note.
func (e *expansion) stopped(ctx context.Context) bool {
	if err := ctx.Err(); err != nil {
		e.note("stopped: %v", err)
		return true
	}
	return false
}

// document returns path's document, fetching and parsing it once; false when
// it is not available, which has been noted.
func (e *expansion) document(ctx context.Context, path string) (*cached, bool) {
	if doc, seen := e.docs[path]; seen {
		return doc, doc.ok
	}
	if e.fetches >= MaxFetches {
		if !e.limitNoted {
			e.note("fetch limit of %d documents reached; later rows on unfetched documents skipped", MaxFetches)
			e.limitNoted = true
		}
		return nil, false
	}
	e.fetches++
	doc := &cached{}
	fetched, err := e.fetch(ctx, path)
	if err != nil {
		e.note("%s: %v", path, err)
	} else {
		doc.Document, doc.headings, doc.ok = fetched, mdoutline.Headings(fetched.Body), true
	}
	e.docs[path] = doc
	return doc, doc.ok
}

// render prints the blocks one row asks for and returns how many it printed.
// A whole document whose H1 opens it is pinned to that H1's anchor, so the
// block can be cited as a section, as the answer contract asks.
func (e *expansion) render(path, anchor string, doc *cached) int {
	body := doc.Body
	p := piece{path: path, anchor: anchor, version: doc.Version}
	if anchor == "" && len(body) >= mdoutline.OutlineThreshold {
		n := 0
		for _, h := range matchingSections(body, doc.headings, e.terms) {
			p.anchor, p.start, p.end, p.text = h.Anchor, h.Start, h.End, body[h.Start:h.End]
			if e.emit(&p) {
				n++
			}
		}
		if n == 0 {
			p.anchor, p.start, p.end, p.text = "", 0, 0, mdoutline.OutlineBody(path, body)
			if e.emit(&p) {
				n++
			}
		}
		return n
	}
	p.start, p.end, p.text = 0, len(body), body
	if anchor == "" {
		p.anchor = openingAnchor(body, doc.headings)
	} else {
		h, found := mdoutline.Find(doc.headings, anchor)
		if !found {
			e.note("%s: section #%s not found", path, anchor)
			return 0
		}
		p.start, p.end, p.text = h.Start, h.End, body[h.Start:h.End]
	}
	if e.emit(&p) {
		return 1
	}
	return 0
}

// emit prints one piece unless an earlier block already covers its range or
// repeats its text; a repeat prints as a stub naming the first location so
// the provenance survives without a second copy.
func (e *expansion) emit(p *piece) bool {
	loc := pinned(p.path, p.anchor, p.version)
	if p.end > p.start {
		for _, s := range e.spans[p.path] {
			if p.start < s.end && s.start < p.end {
				if p.start >= s.start && p.end <= s.end {
					e.note("%s is within %s above", loc, s.loc)
				} else {
					e.note("%s encloses %s above; skipped", loc, s.loc)
				}
				return false
			}
		}
	}
	text := strings.TrimSpace(p.text)
	first, dup := e.texts[text]
	if dup {
		text = "same text as " + first
	}
	if !e.add(loc, text) {
		if len(e.unfit) < maxUnfitListed {
			e.unfit = append(e.unfit, fmt.Sprintf("%s (~%d tokens)", loc, len(text)/bytesPerToken))
		} else {
			e.unfitMore++
		}
		return false
	}
	if !dup {
		e.texts[text] = loc
	}
	// A stub covers its range too, so a nested row is not printed after it.
	if p.end > p.start {
		e.spans[p.path] = append(e.spans[p.path], span{start: p.start, end: p.end, loc: loc})
	}
	return true
}

// relations prints the document's rel-* declarations once, grouped by
// predicate on one line under its block, and returns them. Malformed values
// are skipped as the crawl skips them (ADR 0004).
func (e *expansion) relations(path string, doc *cached) []graph.RelRef {
	if e.linked[path] {
		return nil
	}
	e.linked[path] = true
	refs := graph.RelEdges(path, doc.Metadata).Refs
	if len(refs) == 0 {
		return nil
	}
	var groups []string
	last := ""
	for _, ref := range refs {
		target := ref.Target
		if ref.Fragment != "" {
			target += "#" + ref.Fragment
		}
		if ref.Rel == last {
			groups[len(groups)-1] += ", " + target
			continue
		}
		groups, last = append(groups, "rel-"+ref.Rel+" "+target), ref.Rel
	}
	e.write(Delimiter + " related: " + strings.Join(groups, "; ") + "\n\n")
	return refs
}

// preferFirst orders candidates so PreferredRelations come before the rest,
// keeping declaration order within each group.
func preferFirst(candidates []graph.RelRef) []graph.RelRef {
	slices.SortStableFunc(candidates, func(a, b graph.RelRef) int {
		pa, pb := slices.Contains(PreferredRelations, a.Rel), slices.Contains(PreferredRelations, b.Rel)
		switch {
		case pa == pb:
			return 0
		case pa:
			return -1
		default:
			return 1
		}
	})
	return candidates
}

// expandRelated follows source's queued relations one hop, within source's
// authority, skipping documents the table already names.
func (e *expansion) expandRelated(ctx context.Context, source string, candidates []graph.RelRef, inTable map[string]bool) {
	seen := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if e.related >= MaxRelated || e.remaining <= 0 || e.stopped(ctx) {
			return
		}
		if seen[c.Target] || inTable[c.Target] || !sameAuthority(source, c.Target) {
			continue
		}
		seen[c.Target] = true
		doc, ok := e.document(ctx, c.Target)
		if !ok {
			continue
		}
		if e.render(c.Target, c.Fragment, doc) > 0 {
			e.related++
			e.relations(c.Target, doc)
		}
	}
}

// unfitNote names the rows that did not fit, so the reader can fetch one.
func (e *expansion) unfitNote() {
	if len(e.unfit) == 0 {
		return
	}
	more := ""
	if e.unfitMore > 0 {
		more = fmt.Sprintf(" +%d more", e.unfitMore)
	}
	e.note("did not fit: %s%s", strings.Join(e.unfit, ", "), more)
}

// sameAuthority reports whether target lives where source does: both bare
// paths, or the same server identity.
func sameAuthority(source, target string) bool {
	sourceAuthority, sourceOK := authority(source)
	targetAuthority, targetOK := authority(target)
	return sourceOK && targetOK && sourceAuthority == targetAuthority
}

// authority is the server identity of a location (links.Target's rule);
// "" for a bare path.
func authority(loc string) (string, bool) {
	if !strings.Contains(loc, "://") {
		return "", strings.HasPrefix(loc, "/")
	}
	target, err := links.ParseMark(loc)
	if err != nil {
		return "", false
	}
	return target.AuthorityURL(), true
}

// openingAnchor is the anchor of an H1 that opens body and spans all of it;
// "" when the document has none, so the block stays a whole-document one.
func openingAnchor(body string, headings []mdoutline.Heading) string {
	if len(headings) == 0 {
		return ""
	}
	h := headings[0]
	if h.Level != 1 || strings.TrimSpace(body[:h.Start]) != "" || h.End < len(strings.TrimRight(body, "\n")) {
		return ""
	}
	return h.Anchor
}

// escapeFrame indents body lines that would read as a frame line.
func escapeFrame(text string) string {
	if !strings.Contains(text, Delimiter) {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, Delimiter) {
			lines[i] = " " + ln
		}
	}
	return strings.Join(lines, "\n")
}

// queryTerms are the lowercase query words of two or more characters.
func queryTerms(query string) []string {
	var terms []string
	for t := range strings.FieldsSeq(strings.ToLower(query)) {
		if t = strings.Trim(t, "\"'.,;:()"); len(t) >= 2 {
			terms = append(terms, t)
		}
	}
	return terms
}

// matchingSections returns the H2-or-deeper sections whose text holds every
// term (case-insensitive substrings), outermost only, in order.
func matchingSections(body string, headings []mdoutline.Heading, terms []string) []mdoutline.Heading {
	if len(terms) == 0 {
		return nil
	}
	var out []mdoutline.Heading
	lastEnd := 0
	for _, h := range headings {
		if h.Level < 2 || h.Start < lastEnd {
			continue
		}
		text := strings.ToLower(body[h.Start:h.End])
		hit := true
		for _, t := range terms {
			if !strings.Contains(text, t) {
				hit = false
				break
			}
		}
		if hit {
			out = append(out, h)
			lastEnd = h.End
		}
	}
	return out
}

// locations returns the first cell of every data row, in table order, split
// into path and anchor.
func locations(table string) []row {
	var out []row
	for line := range strings.SplitSeq(table, "\n") {
		cells, ok := lookuptable.SplitRow(line)
		if !ok || !lookuptable.IsDataRow(cells) {
			continue
		}
		path, anchor := lookuptable.SplitLocation(lookuptable.Unescape(cells[0]))
		out = append(out, row{path: path, anchor: anchor})
	}
	return out
}
