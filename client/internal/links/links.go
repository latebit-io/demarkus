// Package links extracts and resolves links from markdown documents.
package links

import (
	"net/url"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Extract parses body as markdown and returns all non-fragment link destinations.
func Extract(body string) []string {
	src := []byte(body)
	reader := text.NewReader(src)
	doc := goldmark.DefaultParser().Parse(reader)

	var links []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		link, ok := n.(*ast.Link)
		if !ok {
			return ast.WalkContinue, nil
		}
		dest := string(link.Destination)
		if dest != "" && !strings.HasPrefix(dest, "#") {
			links = append(links, dest)
		}
		return ast.WalkContinue, nil
	})
	return links
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

// ExtractTitle returns the text of the first top-level heading in the markdown body.
// Returns empty string if no heading is found.
func ExtractTitle(body string) string {
	src := []byte(body)
	reader := text.NewReader(src)
	doc := goldmark.DefaultParser().Parse(reader)

	var title string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		heading, ok := n.(*ast.Heading)
		if !ok || heading.Level != 1 {
			return ast.WalkContinue, nil
		}
		title = string(heading.Text(src))
		return ast.WalkStop, nil
	})
	return title
}
