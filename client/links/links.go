// Package links extracts and resolves links from markdown documents.
package links

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/latebit-io/demarkus/protocol"
)

const markScheme = "mark://"

// defaultPort is protocol.DefaultPort as a string, for URL port comparison.
var defaultPort = strconv.Itoa(protocol.DefaultPort)

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

// Resolve resolves a possibly-relative link dest against baseURL.
func Resolve(baseURL, dest string) string {
	if strings.Contains(dest, "://") {
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
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	host := u.Hostname()
	if host == "" {
		return raw
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6 literal; Hostname() strips the brackets
	}
	if port := u.Port(); port != "" && port != defaultPort {
		host += ":" + port
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return markScheme + host + path
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
	Text         string // rendered link text
	OpenBracket  int    // byte offset of '[' in source, -1 for links with no text nodes
	CloseBracket int    // byte offset of ']' in source, -1 for links with no text nodes
	BlockStart   int    // byte offset of the enclosing block's start, -1 if unknown; section-level position even when the brackets are unknown
}

// ExtractWithPositions parses body as markdown and returns link info with source positions.
// Same filtering as Extract (excludes fragment-only, strips fragments from destinations).
func ExtractWithPositions(body string) []LinkInfo {
	src := []byte(body)
	reader := text.NewReader(src)
	doc := goldmark.DefaultParser().Parse(reader)

	var infos []LinkInfo
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		dest := string(link.Destination)
		if dest == "" || strings.HasPrefix(dest, "#") {
			return ast.WalkContinue, nil
		}
		if idx := strings.Index(dest, "#"); idx != -1 {
			dest = dest[:idx]
		}

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

		// Find the byte range of the link text by collecting descendant text segments.
		firstStart, lastStop := -1, -1
		var linkText strings.Builder
		_ = ast.Walk(link, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			t, ok := child.(*ast.Text)
			if !ok {
				return ast.WalkContinue, nil
			}
			seg := t.Segment
			linkText.Write(seg.Value(src))
			if firstStart < 0 {
				firstStart = seg.Start
			}
			lastStop = seg.Stop
			return ast.WalkContinue, nil
		})

		if firstStart < 0 {
			// Link with no text nodes (e.g. [](a.md)) — preserve for navigation
			// but mark bracket positions as unknown so marker injection skips it.
			infos = append(infos, LinkInfo{
				Dest:         dest,
				Text:         "",
				OpenBracket:  -1,
				CloseBracket: -1,
				BlockStart:   blockStart,
			})
			return ast.WalkContinue, nil
		}

		// Scan backward from the first text segment to find '['.
		open := firstStart - 1
		for open >= 0 && src[open] != '[' {
			open--
		}
		// Scan forward from the last text segment to find ']'.
		closeBracket := lastStop
		for closeBracket < len(src) && src[closeBracket] != ']' {
			closeBracket++
		}

		infos = append(infos, LinkInfo{
			Dest:         dest,
			Text:         linkText.String(),
			OpenBracket:  open,
			CloseBracket: closeBracket,
			BlockStart:   blockStart,
		})
		return ast.WalkContinue, nil
	})
	return infos
}

// ExtractTitle returns the text of the first top-level heading in the markdown body.
// Returns empty string if no heading is found.
func ExtractTitle(body string) string {
	src := []byte(body)
	reader := text.NewReader(src)
	doc := goldmark.DefaultParser().Parse(reader)

	var title strings.Builder
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		heading, ok := n.(*ast.Heading)
		if !ok || heading.Level != 1 {
			return ast.WalkContinue, nil
		}
		_ = ast.Walk(heading, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			if t, ok := child.(*ast.Text); ok {
				title.Write(t.Value(src))
			}
			return ast.WalkContinue, nil
		})
		return ast.WalkStop, nil
	})
	return title.String()
}
