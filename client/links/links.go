// Package links extracts and resolves links from markdown documents.
package links

import (
	"bytes"
	"cmp"
	"net/url"
	pathpkg "path"
	"regexp"
	"slices"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

const markScheme = "mark://"

// Extract parses body as markdown and returns all link destinations,
// excluding fragment-only links. Fragments are stripped from destinations
// so that doc.md#section is returned as doc.md.
func Extract(body string) []string {
	infos := ExtractWithPositions(body)
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Dest
	}
	return out
}

// isAbsoluteURL reports a dest with a scheme (https://, mark://, mailto:).
func isAbsoluteURL(dest string) bool {
	u, err := url.Parse(dest)
	return err == nil && u.Scheme != ""
}

// Resolve resolves a possibly-relative link dest against baseURL.
func Resolve(baseURL, dest string) string {
	if isAbsoluteURL(dest) {
		return dest
	}
	base, err := url.Parse(baseURL)
	if err != nil || baseURL == "" {
		return dest
	}
	ref, err := url.Parse(dest)
	if err != nil {
		return dest
	}
	return base.ResolveReference(ref).String()
}

// CanonicalURL returns the graph-identity form of a mark:// URL: the authority
// with the default port removed (ADR 0005). A dial address keeps its port; an
// identity does not. Anything else is returned unchanged.
func CanonicalURL(raw string) string {
	if !strings.HasPrefix(raw, markScheme) {
		return raw
	}
	target, err := ParseMark(raw)
	if err != nil {
		return raw
	}
	return target.NodeURL()
}

// NodeURL builds a graph node identity from a parsed host and path. host may
// carry a dial port; identity drops the default one. Build identities with
// this rather than concatenating, so the rule lives in exactly one place.
func NodeURL(host, path string) string {
	if path == "" {
		path = "/"
	}
	return CanonicalURL(markScheme + host + path)
}

// LinkInfo describes a link extracted from markdown, including its position in the source.
type LinkInfo struct {
	Dest         string // link destination (fragment stripped)
	Fragment     string // destination fragment without '#', "" when the link has none
	Text         string // rendered link text
	OpenBracket  int    // byte offset of '[' in source, -1 for links with no text nodes
	CloseBracket int    // byte offset of ']' in source, -1 for links with no text nodes
	BlockStart   int    // byte offset of the enclosing block's start, -1 if unknown; section-level position even when the brackets are unknown
	End          int    // byte offset just past an inline link's "(dest)", -1 for other links
}

// Pos is the link's position in the source for section attribution: its
// opening bracket, else its block's start, -1 when neither is known.
func (l *LinkInfo) Pos() int {
	if l.OpenBracket >= 0 {
		return l.OpenBracket
	}
	return l.BlockStart
}

// ExtractWithPositions parses body as markdown and returns link info with source positions.
// Same filtering as Extract (excludes fragment-only, strips fragments from destinations).
func ExtractWithPositions(body string) []LinkInfo {
	src := []byte(body)
	reader := text.NewReader(src)
	doc := goldmark.DefaultParser().Parse(reader)

	var infos []LinkInfo
	walk(doc, func(n ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.WalkContinue
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue
		}
		dest := string(link.Destination)
		if dest == "" || strings.HasPrefix(dest, "#") {
			return ast.WalkContinue
		}
		dest, fragment, _ := strings.Cut(dest, "#")

		blockStart := blockStart(link)
		open, closeBracket, linkText := brackets(link, src)
		if closeBracket < 0 {
			// Link with no text nodes (e.g. [](a.md)) — preserve for navigation
			// but mark bracket positions as unknown so marker injection skips it.
			infos = append(infos, LinkInfo{
				Dest:         dest,
				Fragment:     fragment,
				Text:         "",
				OpenBracket:  -1,
				CloseBracket: -1,
				BlockStart:   blockStart,
				End:          -1,
			})
			return ast.WalkContinue
		}

		infos = append(infos, LinkInfo{
			Dest:         dest,
			Fragment:     fragment,
			Text:         linkText,
			OpenBracket:  open,
			CloseBracket: closeBracket,
			BlockStart:   blockStart,
			End:          inlineEnd(src, closeBracket),
		})
		return ast.WalkContinue
	})
	return infos
}

// blockStart is the start of the nearest block holding n, -1 if unknown.
// Inline nodes carry no segments, but headings cannot occur inside a block,
// so the block's start is in n's section. BaseInline.Lines() panics.
func blockStart(n ast.Node) int {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Type() != ast.TypeBlock {
			continue
		}
		if lines := p.Lines(); lines != nil && lines.Len() > 0 {
			return lines.At(0).Start
		}
	}
	return -1
}

// brackets finds a link's or image's '[' and ']' from its descendant text
// segments, and its rendered text; -1, -1 for one with no text nodes. A
// nested link or image (a badge: [![alt](src)](href)) counts whole.
func brackets(link ast.Node, src []byte) (open, closeBracket int, linkText string) {
	firstStart, lastStop := -1, -1
	var b strings.Builder
	walk(link, func(child ast.Node, entering bool) ast.WalkStatus {
		if !entering || child == link {
			return ast.WalkContinue
		}
		var start, stop int
		status := ast.WalkContinue
		switch v := child.(type) {
		case *ast.Text:
			b.Write(v.Segment.Value(src))
			start, stop = v.Segment.Start, v.Segment.Stop
		case *ast.Link, *ast.Image:
			o, c, inner := brackets(v, src)
			if o < 0 {
				firstStart = -1 // unlocatable inside: so is link
				return ast.WalkStop
			}
			b.WriteString(inner)
			start, stop, status = o, linkEnd(src, v, c), ast.WalkSkipChildren
			if v.Kind() == ast.KindImage {
				start-- // its '!'
			}
		default:
			return ast.WalkContinue
		}
		if firstStart < 0 {
			firstStart = start
		}
		lastStop = stop
		return status
	})
	if firstStart < 0 {
		return -1, -1, ""
	}
	open = firstStart - 1
	for open >= 0 && src[open] != '[' {
		open--
	}
	closeBracket = lastStop
	for closeBracket < len(src) && src[closeBracket] != ']' {
		closeBracket++
	}
	return open, closeBracket, b.String()
}

// linkEnd is the offset just past link, whose ']' is at closeBracket: past
// its "(dest)" or "[label]", or the ']' itself for a shortcut.
func linkEnd(src []byte, link ast.Node, closeBracket int) int {
	var ref *ast.ReferenceLink
	switch v := link.(type) {
	case *ast.Link:
		ref = v.Reference
	case *ast.Image:
		ref = v.Reference
	}
	switch {
	case ref == nil:
		if end := inlineEnd(src, closeBracket); end >= 0 {
			return end
		}
	case ref.Type != ast.ReferenceLinkShortcut:
		if j := bytes.IndexByte(src[closeBracket+1:], ']'); j >= 0 {
			return closeBracket + 1 + j + 1
		}
	}
	return closeBracket + 1
}

// Span is a destination rewritable in place: an inline link's or image's,
// or a reference definition's; [Start, End) are its bytes.
type Span struct {
	Dest       string
	Start, End int
}

// Spans returns body's rewritable destinations in source order, a badge's
// image before its link; none in code, for a reference use, an escape, or a
// link with no text to find it by. A span excludes any title.
func Spans(body string) []Span {
	src := []byte(body)
	var out []Span
	walk(goldmark.DefaultParser().Parse(text.NewReader(src)), func(n ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.WalkContinue
		}
		var sp Span
		ok := false
		switch v := n.(type) {
		case *ast.Link:
			if v.Reference == nil {
				sp, ok = inlineSpan(src, v, v.Destination)
			}
		case *ast.Image:
			if v.Reference == nil {
				sp, ok = inlineSpan(src, v, v.Destination)
			}
		case *ast.LinkReferenceDefinition:
			sp, ok = definitionSpan(src, v)
		}
		if ok {
			out = append(out, sp)
		}
		return ast.WalkContinue
	})
	slices.SortFunc(out, func(a, b Span) int { return cmp.Compare(a.Start, b.Start) })
	return out
}

// inlineSpan finds an inline link's or image's destination in its "(dest)".
func inlineSpan(src []byte, n ast.Node, dest []byte) (Span, bool) {
	if _, closeBracket, _ := brackets(n, src); closeBracket >= 0 && inlineEnd(src, closeBracket) >= 0 {
		return verbatim(src, closeBracket+2, " \t\n)", dest) // past "]("
	}
	return Span{}, false
}

// definitionSpan finds a reference definition's destination after its "]:".
func definitionSpan(src []byte, d *ast.LinkReferenceDefinition) (Span, bool) {
	lines := d.Lines()
	if lines.Len() == 0 {
		return Span{}, false
	}
	from, to := lines.At(0).Start, lines.At(lines.Len()-1).Stop
	i := bytes.IndexByte(src[from:to], '[')
	if i < 0 {
		return Span{}, false
	}
	for i += from + 1; i < to && src[i] != ']'; i++ {
		if src[i] == '\\' {
			i++
		}
	}
	if i+1 >= to || src[i+1] != ':' {
		return Span{}, false
	}
	return verbatim(src[:to], i+2, " \t\n", d.Destination)
}

// verbatim is dest's span when its bytes follow from, past blanks: up to a
// stop byte, or the '>' of a "<dest>". An escaped or entity-coded destination
// differs from its source text: rewriting it would touch other bytes.
func verbatim(src []byte, from int, stops string, dest []byte) (Span, bool) {
	for from < len(src) && strings.IndexByte(" \t\n", src[from]) >= 0 {
		from++
	}
	if from < len(src) && src[from] == '<' {
		from, stops = from+1, ">\n"
	}
	end := from
	for end < len(src) && strings.IndexByte(stops, src[end]) < 0 {
		end++
	}
	if !bytes.Equal(src[from:end], dest) {
		return Span{}, false
	}
	return Span{Dest: string(dest), Start: from, End: end}, true
}

// Definition is a reference definition as written, its container's prefixes
// (a quote's "> ") dropped; Pos is where it starts.
type Definition struct {
	Label, Text string
	Pos         int
}

// Definitions returns body's reference definitions in document order.
func Definitions(body string) []Definition {
	src := []byte(body)
	var out []Definition
	walk(goldmark.DefaultParser().Parse(text.NewReader(src)), func(n ast.Node, entering bool) ast.WalkStatus {
		if d, ok := n.(*ast.LinkReferenceDefinition); ok && entering && d.Lines().Len() > 0 {
			var b strings.Builder
			for i := range d.Lines().Len() {
				seg := d.Lines().At(i)
				b.Write(seg.Value(src))
			}
			out = append(out, Definition{Label: string(d.Label), Text: b.String(), Pos: d.Lines().At(0).Start})
		}
		return ast.WalkContinue
	})
	return out
}

// htmlRef is an href or src attribute in raw HTML, its value in any quoting.
var htmlRef = regexp.MustCompile(`(?i)\b(?:href|src)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)

// Followed is a destination a reader can follow: Pos is the start of the
// block it sits in, Label the reference it resolves through, "" for none.
type Followed struct {
	Dest, Label string
	Pos         int
}

// Targets is every destination a reader can follow in body, in document
// order: links and images inline or by reference, autolinks, and href or src
// in raw HTML. Unlike Extract it keeps fragments and fragment-only links.
func Targets(body string) []Followed {
	src := []byte(body)
	var out []Followed
	walk(goldmark.DefaultParser().Parse(text.NewReader(src)), func(n ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.WalkContinue
		}
		var html []byte
		pos := -1
		switch v := n.(type) {
		case *ast.Link:
			out = append(out, Followed{Dest: string(v.Destination), Label: refLabel(v.Reference), Pos: blockStart(v)})
		case *ast.Image:
			out = append(out, Followed{Dest: string(v.Destination), Label: refLabel(v.Reference), Pos: blockStart(v)})
		case *ast.AutoLink:
			out = append(out, Followed{Dest: string(v.URL(src)), Pos: blockStart(v)})
		case *ast.RawHTML:
			pos = blockStart(v)
			for i := range v.Segments.Len() {
				seg := v.Segments.At(i)
				html = append(html, seg.Value(src)...)
			}
		case *ast.HTMLBlock:
			for i := range v.Lines().Len() {
				line := v.Lines().At(i)
				if i == 0 {
					pos = line.Start
				}
				html = append(html, line.Value(src)...)
			}
			if v.HasClosure() {
				html = append(html, v.ClosureLine.Value(src)...)
			}
		}
		for _, m := range htmlRef.FindAllSubmatch(html, -1) {
			out = append(out, Followed{Dest: string(bytes.Join(m[1:], nil)), Pos: pos})
		}
		return ast.WalkContinue
	})
	return out
}

// LabelKey is a reference label as matching compares it: case folded,
// inner whitespace collapsed.
func LabelKey(label string) string {
	return util.ToLinkReference([]byte(label))
}

// refLabel is the label a reference link resolves through, "" for inline.
func refLabel(r *ast.ReferenceLink) string {
	if r == nil {
		return ""
	}
	return string(r.Value)
}

// walk is ast.Walk for a callback that cannot fail.
func walk(n ast.Node, visit func(n ast.Node, entering bool) ast.WalkStatus) {
	err := ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		return visit(n, entering), nil
	})
	if err != nil {
		// Unreachable: ast.Walk fails only when its callback does.
		panic("links: ast.Walk: " + err.Error())
	}
}

// inlineEnd is the offset just past the "(dest)" that follows the ']' at
// closeBracket; -1 when none does (a reference-style link).
func inlineEnd(src []byte, closeBracket int) int {
	i := closeBracket + 1
	if i >= len(src) || src[i] != '(' {
		return -1
	}
	depth := 0
	for ; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '\n':
			return -1
		}
	}
	return -1
}

// ExtractTitle returns the text of the first top-level heading in the markdown body.
// Returns empty string if no heading is found.
func ExtractTitle(body string) string {
	src := []byte(body)
	reader := text.NewReader(src)
	doc := goldmark.DefaultParser().Parse(reader)

	var title strings.Builder
	walk(doc, func(n ast.Node, entering bool) ast.WalkStatus {
		if !entering {
			return ast.WalkContinue
		}
		heading, ok := n.(*ast.Heading)
		if !ok || heading.Level != 1 {
			return ast.WalkContinue
		}
		walk(heading, func(child ast.Node, entering bool) ast.WalkStatus {
			if !entering {
				return ast.WalkContinue
			}
			if t, ok := child.(*ast.Text); ok {
				title.Write(t.Value(src))
			}
			return ast.WalkContinue
		})
		return ast.WalkStop
	})
	return title.String()
}

// Resolver resolves a document's links to paths on one store. The zero
// value knows no authority, so every mark:// link is external to it.
type Resolver struct{ authority string }

// ResolverFor binds the store the surface names as host ("host:port", or a
// world name behind a broker), parsed once; "" binds none.
func ResolverFor(host string) Resolver {
	if host == "" {
		return Resolver{}
	}
	return Resolver{authority: AuthorityURL(host)}
}

// Path is ResolvePath that also reads a mark:// link to the bound store as
// a path.
func (r Resolver) Path(docPath, dest string) string {
	if p := ResolvePath(docPath, dest); p != "" || r.authority == "" || len(dest) < len(markScheme) || !strings.EqualFold(dest[:len(markScheme)], markScheme) {
		return p
	}
	t, err := ParseMark(dest)
	if err != nil || t.AuthorityURL() != r.authority {
		return ""
	}
	return cleanPath(docPath, t.Path)
}

// Self reports a destination, fragment cut, that names docPath itself: ""
// (a bare "#a"), a query, a path, or a mark:// URL on r's store.
func (r Resolver) Self(docPath, dest string) bool {
	return dest == "" || strings.HasPrefix(dest, "?") || r.Path(docPath, dest) == docPath
}

// ResolvePath is the store path a link in docPath points at: fragment and
// query dropped, absolute cleaned, relative joined to the document's
// directory, a trailing slash kept. "" for an external link.
func ResolvePath(docPath, dest string) string {
	dest, _, _ = strings.Cut(dest, "#")
	dest, _, _ = strings.Cut(dest, "?")
	if dest == "" || isAbsoluteURL(dest) {
		return ""
	}
	return cleanPath(docPath, dest)
}

// cleanPath joins a path link to the document's directory.
func cleanPath(docPath, dest string) string {
	target := pathpkg.Clean(dest)
	if !strings.HasPrefix(dest, "/") {
		target = pathpkg.Join(pathpkg.Dir(docPath), dest)
	}
	if strings.HasSuffix(dest, "/") && !strings.HasSuffix(target, "/") {
		target += "/"
	}
	return target
}
