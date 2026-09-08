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
	hubStatusKey = regexp.MustCompile(`(?i)\bstatus:`)
	hubISODate   = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	hubPRNumber  = regexp.MustCompile(`#\d+\b`)
)

// bullet is one list item as the parser resolves it: visible text (link
// text kept, destinations dropped), the text outside links and code spans,
// whether it opens with a link, carries bold, or holds further blocks.
type bullet struct {
	visible    string
	outside    string
	label      string
	opensLink  bool
	bold       bool
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
			bad = append(bad, fmt.Sprintf("%q", b.label))
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
		if b.opensLink {
			n++
		}
	}
	return n >= hubMinLinkItems && textLines > 0 && float64(n) >= hubLinkShare*float64(textLines)
}

// bulletOK: one paragraph, under the visible cap, and no status marker
// outside links and code spans (a journal hub links by date; a rule page
// quotes `Status:`).
func bulletOK(b bullet) bool {
	if b.multiBlock || b.bold {
		return false
	}
	if utf8.RuneCountInString(b.visible) > hubBulletMax {
		return false
	}
	return !hubStatusKey.MatchString(b.outside) && !hubISODate.MatchString(b.outside) && !hubPRNumber.MatchString(b.outside)
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

// bulletOf reads one list item from its first text block; nested lists are
// not blocks of their own, anything else after the first block is.
func bulletOf(item *ast.ListItem, src []byte) bullet {
	var b bullet
	blocks := 0
	for c := item.FirstChild(); c != nil; c = c.NextSibling() {
		if _, nested := c.(*ast.List); nested {
			continue
		}
		blocks++
		if blocks > 1 {
			continue
		}
		// The parser resolves reference links, so [text][label] and
		// [text] with a definition are links; unresolved ones stay text.
		b.opensLink = opensWithLink(c.FirstChild())
		var r inlineReader
		r.walk(c, src, 0, 0)
		b.visible = strings.TrimSpace(r.visible.String())
		b.outside = r.outside.String()
		b.bold = r.bold
		b.label = r.label
		if b.label == "" {
			b.label = b.visible
			if rs := []rune(b.label); len(rs) > 40 {
				b.label = string(rs[:40]) + "..."
			}
		}
	}
	b.multiBlock = blocks > 1
	return b
}

// opensWithLink descends through leading emphasis wrappers to the first
// real inline node and reports whether it is a link.
func opensWithLink(n ast.Node) bool {
	for {
		switch v := n.(type) {
		case *ast.Link, *ast.AutoLink:
			return true
		case *ast.Emphasis:
			n = v.FirstChild()
		default:
			return false
		}
	}
}

// inlineReader flattens an inline tree: visible text for the length rule,
// the text outside links and code spans for the marker rules, bold seen
// outside links, and the first link's text as the bullet's label.
type inlineReader struct {
	visible, outside strings.Builder
	bold             bool
	label            string
	labelDone        bool
}

func (r *inlineReader) walk(n ast.Node, src []byte, inLink, inCode int) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch v := c.(type) {
		case *ast.Text:
			s := string(v.Segment.Value(src))
			if v.SoftLineBreak() || v.HardLineBreak() {
				s += " "
			}
			r.visible.WriteString(s)
			if inLink == 0 && inCode == 0 {
				r.outside.WriteString(s)
			}
		case *ast.CodeSpan:
			r.walk(v, src, inLink, inCode+1)
		case *ast.Link:
			start := r.visible.Len()
			r.walk(v, src, inLink+1, inCode)
			if !r.labelDone {
				r.label, r.labelDone = strings.TrimSpace(r.visible.String()[start:]), true
			}
		case *ast.AutoLink:
			r.visible.Write(v.URL(src))
		case *ast.Emphasis:
			// Bold counts only when it adds text outside links: a bold
			// link label is a link, not a status marker.
			before := r.outside.Len()
			r.walk(v, src, inLink, inCode)
			if v.Level >= 2 && r.outside.Len() > before {
				r.bold = true
			}
		default:
			r.walk(v, src, inLink, inCode)
		}
	}
}
