// Package links extracts and resolves links from markdown documents.
package links

import (
	"bytes"
	"net/url"
	pathpkg "path"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
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

		// Inline nodes carry no segments, but the enclosing block does;
		// headings cannot occur inside a block, so its start is in the same
		// section as the link. Skip inline ancestors (emphasis wrapping the
		// link, etc.): BaseInline.Lines() panics by contract.
		blockStart := -1
		for p := link.Parent(); p != nil; p = p.Parent() {
			if p.Type() != ast.TypeBlock {
				continue
			}
			if lines := p.Lines(); lines != nil && lines.Len() > 0 {
				blockStart = lines.At(0).Start
				break
			}
		}

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

// brackets finds a link's '[' and ']' from its descendant text segments, and
// its rendered text; -1, -1 for a link with no text nodes.
func brackets(link *ast.Link, src []byte) (open, closeBracket int, linkText string) {
	firstStart, lastStop := -1, -1
	var b strings.Builder
	walk(link, func(child ast.Node, entering bool) ast.WalkStatus {
		if t, ok := child.(*ast.Text); ok && entering {
			b.Write(t.Segment.Value(src))
			if firstStart < 0 {
				firstStart = t.Segment.Start
			}
			lastStop = t.Segment.Stop
		}
		return ast.WalkContinue
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

// FragmentLink is an inline same-document link ("[x](#a)"): its anchor and
// the byte span [Start, End) of the "#a" destination in the source.
type FragmentLink struct {
	Anchor     string
	Start, End int
}

// FragmentLinks returns the inline fragment-only links in body, the ones
// ExtractWithPositions drops. The parser skips code and escaped brackets,
// and a span excludes any title, so rewriting it keeps the rest verbatim.
func FragmentLinks(body string) []FragmentLink {
	return anchorSpans(body, func(dest string) bool { return dest == "" })
}

// SelfLinks is FragmentLinks plus the links that name docPath before the
// fragment ("/d.md#a", "d.md#a"); each span is still just the "#a".
func SelfLinks(body, docPath string) []FragmentLink {
	return anchorSpans(body, func(dest string) bool { return dest == "" || ResolvePath(docPath, dest) == docPath })
}

// anchorSpans returns the "#a" span of every inline link whose destination
// before the fragment keep accepts.
func anchorSpans(body string, keep func(dest string) bool) []FragmentLink {
	src := []byte(body)
	var out []FragmentLink
	walk(goldmark.DefaultParser().Parse(text.NewReader(src)), func(n ast.Node, entering bool) ast.WalkStatus {
		link, ok := n.(*ast.Link)
		if !entering || !ok {
			return ast.WalkContinue
		}
		dest, _, hasFragment := bytes.Cut(link.Destination, []byte("#"))
		if !hasFragment || !keep(string(dest)) {
			return ast.WalkContinue
		}
		_, closeBracket, _ := brackets(link, src)
		if closeBracket < 0 || inlineEnd(src, closeBracket) < 0 {
			return ast.WalkContinue
		}
		start := closeBracket + 2 // past "]("
		for start < len(src) && (src[start] == ' ' || src[start] == '\t' || src[start] == '\n' || src[start] == '<') {
			start++
		}
		end := start
		for end < len(src) && !strings.ContainsRune(" \t\n)>", rune(src[end])) {
			end++
		}
		// An escaped or entity-coded destination differs from its source
		// text; leave it rather than rewrite bytes that are not the anchor.
		if bytes.Equal(src[start:end], link.Destination) {
			hash := start + len(dest)
			out = append(out, FragmentLink{Anchor: string(src[hash+1 : end]), Start: hash, End: end})
		}
		return ast.WalkContinue
	})
	return out
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
