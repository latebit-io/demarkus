// Package outline sizes and renders outline-mode bodies and anchors links to
// their sections. Splitting and slugging are protocol/mdoutline's.
package outline

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/protocol/mdoutline"
)

// Threshold is the body size (bytes) at or above which a plain fetch answers
// with an outline, and the size a document splits at to stay one fetch.
const Threshold = 8 * 1024

// Body is the outline-mode body for a large document: heading tree, opening
// paragraph, and the hint for more. docURL is echoed into the hint in the
// consumer's URL shape (bare path, mark://host/path or mark://world/path).
func Body(docURL, body string) string {
	var b strings.Builder
	if tree := mdoutline.Outline(body); tree != "" {
		b.WriteString(tree)
	} else {
		b.WriteString("(document has no headings)\n")
	}
	if para := mdoutline.OpeningParagraph(body); para != "" {
		b.WriteString("\n")
		b.WriteString(para)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nfetch %s#<anchor> for a section; force=true for the full body\n", docURL)
	return b.String()
}
