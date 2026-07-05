// Package mdoutline provides pure functions over markdown bodies for
// heading-tree outlines and #section slicing. It is the shared slicing
// convention for MCP fetch surfaces: anchors match GitHub-style slugs so a
// url#section reference means the same thing across every consumer.
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
// order. Setext headings are included; # lines inside code fences and
// indented code blocks are not headings and are excluded. Anchors follow
// the GitHub slug convention with duplicate suffixing.
//
// Each exported helper reparses body independently, and the End/Lines pass
// below is O(n²) in heading count. Both are deliberate: bodies are capped
// by document size (tens of KB, dozens of headings), where a goldmark parse
// is sub-millisecond, so sharing parsed state across calls would buy
// nothing measurable at the cost of a stateful API.
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
			// Empty heading ("#" with no text) — nothing to slice or anchor.
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

	// Deduplicate against every anchor already taken — generated suffixes
	// count, so "Notes", "Notes", "Notes-1" yields notes, notes-1, notes-1-1
	// (matching GitHub's slugger).
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

// headingText collects the text content of a heading node, descending into
// inline children (code spans, emphasis) the way GitHub does when slugging.
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

// Slug converts heading text to a GitHub-style anchor: lowercase, spaces
// become hyphens, and everything that is not a letter, number, underscore,
// or hyphen is removed. It does not apply duplicate suffixes — Headings
// handles that across a whole document.
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

// Section returns the slice of body opened by the heading whose anchor
// matches anchor (leading '#' optional, matched case-insensitively). The
// slice runs from the heading line through the end of its subtree — up to
// the next heading of the same or shallower level. When no anchor matches
// exactly, the anchor is re-slugged so agents can pass raw heading text
// ("Problem Statement") instead of the slug. Returns false if nothing
// matches.
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

// Outline renders the heading tree as an indented list, one heading per
// line with its anchor and section line count — the map an agent needs to
// pick a #section to fetch. Returns "" for a body with no headings.
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

// Anchors returns just the anchor strings of every heading in body, in
// document order — for "section not found" hints.
func Anchors(body string) []string {
	hs := Headings(body)
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Anchor
	}
	return out
}

// countLines counts the lines covered by span, treating a trailing partial
// line as a line.
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
