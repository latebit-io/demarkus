package marktools

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/links"
)

// Render helpers for the fetch, resource and explore bodies.

// linkLines extracts the document's links as one-line entries with titles,
// deduplicated by destination in document order.
func linkLines(body string) []string {
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

// cappedList writes at most limit pre-formatted lines to b and an honest
// "+N more <noun>" marker for the overflow.
func cappedList(b *strings.Builder, lines []string, limit int, noun string) {
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

// binaryBody flags a body that must not be rendered as text. Separate from
// the store's ValidateBody: render-time check, no dependency on storage.
func binaryBody(body string) bool { return !utf8.ValidString(body) }

// nonMarkdownNotice replaces a non-UTF-8 body in the agent-facing render
// paths; rendering the raw bytes as text would inject mojibake into the
// model's context.
func nonMarkdownNotice(nBytes int) string {
	return fmt.Sprintf("non-markdown or binary document (%d bytes); not rendered as text. Retrieve the raw bytes with a non-MCP client, e.g. the demarkus CLI.", nBytes)
}
