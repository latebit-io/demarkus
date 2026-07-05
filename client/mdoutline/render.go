package mdoutline

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/links"
)

// This file holds the shared rendering helpers for the MCP fetch/explore
// surfaces. Both the local demarkus-mcp and the broker MCP gateway compose
// their tool responses from these, so outline mode and the explore card
// read identically regardless of transport.

// OutlineBody builds the outline-mode body for a large document: heading
// tree with anchors and line counts, the opening paragraph, and the hint
// telling the agent how to get more. docURL is the URL shape the consumer
// exposes (bare path, mark://host/path, or mark://world/path) — it is
// echoed verbatim into the hint.
func OutlineBody(docURL, body string) string {
	var b strings.Builder
	if tree := Outline(body); tree != "" {
		b.WriteString(tree)
	} else {
		b.WriteString("(document has no headings)\n")
	}
	if para := OpeningParagraph(body); para != "" {
		b.WriteString("\n")
		b.WriteString(para)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nfetch %s#<anchor> for a section; force=true for the full body\n", docURL)
	return b.String()
}

// LinkLines extracts the document's links as one-line entries with titles,
// deduplicated by destination in document order.
func LinkLines(body string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, l := range links.ExtractWithPositions(body) {
		if seen[l.Dest] {
			continue
		}
		seen[l.Dest] = true
		if l.Text != "" {
			out = append(out, fmt.Sprintf("- [%s](%s)", l.Text, l.Dest))
		} else {
			out = append(out, "- "+l.Dest)
		}
	}
	return out
}

// CappedList writes at most limit pre-formatted lines to b and an honest
// "+N more <noun>" marker for the overflow.
func CappedList(b *strings.Builder, lines []string, limit int, noun string) {
	for i, line := range lines {
		if i == limit {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if n := len(lines) - limit; n > 0 {
		fmt.Fprintf(b, "+%d more %s\n", n, noun)
	}
}
