package gate

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/latebit-io/demarkus/client/links"
)

// Hub rules from the style guide's "Hubs" section: a link page stays under
// one fetch, links plus one line each, about forty outbound documents.
const (
	hubSizeLimit    = 8 * 1024 // demarkus-mcp and the broker outline a body at or over this
	hubBulletMax    = 200      // visible characters per bullet, link destinations stripped
	hubLinkMax      = 40       // distinct outbound documents before a second-level hub
	hubMinLinkItems = 5        // fewer link bullets than this is a list, not a link page
	hubLinkShare    = 0.8      // share of text lines that are link bullets on a link page
)

var (
	mdLink       = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdCodeSpan   = regexp.MustCompile("`[^`\n]*`")
	hubBold      = regexp.MustCompile(`\*\*[^*\n]+\*\*|__[^_\n]+__`)
	hubStatusKey = regexp.MustCompile(`(?i)\bstatus:`)
	hubISODate   = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	hubPRNumber  = regexp.MustCompile(`#\d+\b`)
)

// bullet is one list item: its first paragraph joined into a line, and
// whether the item holds further block content beyond nested lists.
type bullet struct {
	text       string
	multiBlock bool
}

// hubProblems applies the hub rules when the document is a hub: index.md at
// any depth, or a body that is mostly link bullets. Reads only the body.
func hubProblems(leaf, body string) []string {
	items, textLines := listItems(body)
	if leaf != "index.md" && !linkPage(items, textLines) {
		return nil
	}
	var problems []string
	if len(body) >= hubSizeLimit {
		problems = append(problems, fmt.Sprintf(
			"hub body is %d KB, at or over the 8 KB fetch threshold, so a plain fetch returns an outline; split it into a link hub plus topic files", len(body)/1024))
	}
	var bad []string
	for _, b := range items {
		if !bulletOK(b) {
			bad = append(bad, fmt.Sprintf("%q", bulletLabel(b.text)))
		}
	}
	if len(bad) > 0 {
		shown := bad
		if len(shown) > 3 {
			shown = shown[:3]
		}
		problems = append(problems, fmt.Sprintf(
			"%d hub bullet(s) run past one line or carry status (bold, Status:, a date, a PR number): %s; a hub is links plus one line each, status and summaries stay in the child",
			len(bad), strings.Join(shown, ", ")))
	}
	if n := outboundDocs(body); n > hubLinkMax {
		problems = append(problems, fmt.Sprintf(
			"%d outbound documents, over the %d a hub carries; add a second-level hub", n, hubLinkMax))
	}
	return problems
}

// linkPage reports whether the body is mostly link bullets.
func linkPage(items []bullet, textLines int) bool {
	n := 0
	for _, b := range items {
		if opensWithLink(b.text) {
			n++
		}
	}
	return n >= hubMinLinkItems && textLines > 0 && float64(n) >= hubLinkShare*float64(textLines)
}

func opensWithLink(s string) bool {
	loc := mdLink.FindStringIndex(s)
	return len(loc) == 2 && loc[0] == 0
}

// bulletOK: one paragraph, under the visible cap with destinations stripped,
// and no status marker outside link text and code spans (a journal hub links
// by date; a rule page quotes `Status:`).
func bulletOK(b bullet) bool {
	if b.multiBlock {
		return false
	}
	visible := mdLink.ReplaceAllString(b.text, "[$1]")
	if utf8.RuneCountInString(visible) > hubBulletMax {
		return false
	}
	outside := mdCodeSpan.ReplaceAllString(mdLink.ReplaceAllString(b.text, ""), "")
	return !hubBold.MatchString(outside) && !hubStatusKey.MatchString(outside) &&
		!hubISODate.MatchString(outside) && !hubPRNumber.MatchString(outside)
}

// bulletLabel names a bullet by its first link text, else its opening words.
func bulletLabel(s string) string {
	if m := mdLink.FindStringSubmatch(s); len(m) == 2 && m[1] != "" {
		return m[1]
	}
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}

// outboundDocs counts distinct link destinations that are documents in the
// graph: mark:// and path links, fragment stripped by links.Extract; web
// links are not documents.
func outboundDocs(body string) int {
	seen := make(map[string]bool)
	for _, d := range links.Extract(body) {
		if d == "" || strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://") || strings.HasPrefix(d, "mailto:") {
			continue
		}
		seen[d] = true
	}
	return len(seen)
}

// listItems parses body once and returns every list item plus the count of
// text lines (paragraphs and list text outside fences and headings), the
// denominator of the link-page share.
func listItems(body string) (items []bullet, textLines int) {
	src := []byte(body)
	doc := goldmark.DefaultParser().Parse(text.NewReader(src))
	// The callback never returns an error, so Walk cannot either.
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Paragraph, *ast.TextBlock:
			textLines += v.Lines().Len()
		case *ast.ListItem:
			items = append(items, bulletOf(v, src))
		}
		return ast.WalkContinue, nil
	})
	return items, textLines
}

func bulletOf(item *ast.ListItem, src []byte) bullet {
	var b bullet
	blocks := 0
	for c := item.FirstChild(); c != nil; c = c.NextSibling() {
		if _, nested := c.(*ast.List); nested {
			continue
		}
		blocks++
		if b.text != "" {
			continue
		}
		lines := c.Lines()
		parts := make([]string, 0, lines.Len())
		for i := range lines.Len() {
			seg := lines.At(i)
			parts = append(parts, strings.TrimSpace(string(seg.Value(src))))
		}
		b.text = strings.Join(parts, " ")
	}
	b.multiBlock = blocks > 1
	return b
}
