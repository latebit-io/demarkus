// Package mdoutline splits a markdown body into heading sections and slugs
// their anchors. It is the one section rule for every surface: the client's
// url#anchor fetch and the server's LOOKUP body match must agree on section
// boundaries, so both call this package and nothing else.
package mdoutline

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Heading describes one heading in a markdown body and the section it opens.
type Heading struct {
	Level  int    // 1-6
	Text   string // rendered heading text (inline markup stripped)
	Anchor string // GitHub-style slug, deduplicated with -1, -2, ... suffixes
	Start  int    // byte offset of the heading's line start
	End    int    // byte offset where the section ends (next heading of level <= Level, or len(body))
	Lines  int    // line count of the section span [Start, End)
}

// Headings parses body as markdown and returns all headings in document
// order: ATX and setext, never # lines inside fences or indented code.
// Each helper reparses; bodies are small enough that sharing state buys nothing.
func Headings(body string) []Heading {
	src := []byte(body)
	doc := goldmark.DefaultParser().Parse(text.NewReader(src))

	var hs []Heading
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}
		lines := h.Lines()
		if lines.Len() == 0 {
			// Empty heading ("#" with no text): nothing to slice or anchor.
			return ast.WalkContinue, nil
		}
		start := lines.At(0).Start
		for start > 0 && src[start-1] != '\n' {
			start--
		}
		hs = append(hs, Heading{
			Level: h.Level,
			Text:  headingText(h, src),
			Start: start,
		})
		return ast.WalkContinue, nil
	})

	// Generated suffixes count as taken, matching GitHub's slugger:
	// "Notes", "Notes", "Notes-1" yields notes, notes-1, notes-1-1.
	used := make(map[string]bool, len(hs))
	for i := range hs {
		s := Slug(hs[i].Text)
		a := s
		for n := 1; used[a]; n++ {
			a = s + "-" + strconv.Itoa(n)
		}
		used[a] = true
		hs[i].Anchor = a
	}

	for i := range hs {
		end := len(src)
		for j := i + 1; j < len(hs); j++ {
			if hs[j].Level <= hs[i].Level {
				end = hs[j].Start
				break
			}
		}
		hs[i].End = end
		hs[i].Lines = countLines(src[hs[i].Start:end])
	}
	return hs
}

// headingText collects the text of a heading node, descending into inline
// children (code spans, emphasis) the way GitHub does when slugging.
func headingText(h *ast.Heading, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(h, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if t, ok := n.(*ast.Text); ok {
			b.Write(t.Segment.Value(src))
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

// Slug converts heading text to a GitHub-style anchor: lowercase, spaces to
// hyphens, everything but letters, numbers, underscore, and hyphen removed.
// Duplicate suffixes are Headings' job, since they depend on the whole body.
func Slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Section returns the heading line and subtree opened by anchor (leading '#'
// optional, case-insensitive). Raw heading text is re-slugged as a fallback so
// agents can pass "Problem Statement" instead of the slug.
func Section(body, anchor string) (string, bool) {
	want := strings.TrimPrefix(strings.TrimSpace(anchor), "#")
	hs := Headings(body)
	for _, h := range hs {
		if strings.EqualFold(h.Anchor, want) {
			return body[h.Start:h.End], true
		}
	}
	slugged := Slug(want)
	for _, h := range hs {
		if h.Anchor == slugged {
			return body[h.Start:h.End], true
		}
	}
	return "", false
}

// Outline renders the heading tree as an indented list with anchors and
// section line counts. Returns "" for a body with no headings.
func Outline(body string) string {
	hs := Headings(body)
	if len(hs) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hs {
		fmt.Fprintf(&b, "%s- %s (#%s, %d lines)\n", strings.Repeat("  ", h.Level-1), h.Text, h.Anchor, h.Lines)
	}
	return b.String()
}

// OpeningParagraph returns the first top-level paragraph of body as raw
// markdown, or "" when the document has none.
func OpeningParagraph(body string) string {
	src := []byte(body)
	doc := goldmark.DefaultParser().Parse(text.NewReader(src))
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		p, ok := n.(*ast.Paragraph)
		if !ok {
			continue
		}
		lines := p.Lines()
		if lines.Len() == 0 {
			return ""
		}
		start := lines.At(0).Start
		stop := lines.At(lines.Len() - 1).Stop
		return strings.TrimSpace(string(src[start:stop]))
	}
	return ""
}

// Anchors returns the anchor of every heading in body, in document order.
func Anchors(body string) []string {
	hs := Headings(body)
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Anchor
	}
	return out
}

// countLines counts the lines in span; a trailing partial line counts.
func countLines(span []byte) int {
	if len(span) == 0 {
		return 0
	}
	n := strings.Count(string(span), "\n")
	if span[len(span)-1] != '\n' {
		n++
	}
	return n
}
