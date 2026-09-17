package gate

import (
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
)

// Exported style checks for the doctor, which audits a stored corpus with
// the same rules the gate applies at write time. Callers that already
// parsed the headings pass them in, so a body is outlined once.

// HubProblems applies the hub rules to a document at url; nil when it is not a hub.
func HubProblems(url, body string) []string {
	return hubProblems(leafOf(url), body)
}

// ShapeProblems applies the summary and heading rules.
func ShapeProblems(url, body string, headings []mdoutline.Heading) []string {
	return shapeProblems(url, body, headings)
}

// DuplicateHeadings returns heading texts whose anchor slugs collide.
func DuplicateHeadings(headings []mdoutline.Heading) []string {
	return duplicateHeadings(headings)
}

// EmDashCount counts em dashes in the raw body, code fences included, as
// the style rule does.
func EmDashCount(body string) int {
	return strings.Count(body, "\u2014")
}

// OpensWithFrontmatter reports a body that starts with a key: value block.
func OpensWithFrontmatter(body string) bool {
	return bodyOpensWithFrontmatter(body)
}

// NavExempt reports the navigation leaves (index.md, log.md) the type and
// summary rules skip.
func NavExempt(leaf string) bool {
	return navExempt(leaf)
}
