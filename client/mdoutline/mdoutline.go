// Package mdoutline renders outline and explore bodies for the MCP fetch
// surfaces. Section splitting and slugging live in protocol/mdoutline, the
// one rule the server's LOOKUP body match shares; this package re-exports
// them so existing callers keep one import.
package mdoutline

import "github.com/latebit-io/demarkus/protocol/mdoutline"

// Heading is protocol/mdoutline.Heading.
type Heading = mdoutline.Heading

// Headings is protocol/mdoutline.Headings.
func Headings(body string) []Heading { return mdoutline.Headings(body) }

// Slug is protocol/mdoutline.Slug.
func Slug(heading string) string { return mdoutline.Slug(heading) }

// Section is protocol/mdoutline.Section.
func Section(body, anchor string) (string, bool) { return mdoutline.Section(body, anchor) }

// Outline is protocol/mdoutline.Outline.
func Outline(body string) string { return mdoutline.Outline(body) }

// OpeningParagraph is protocol/mdoutline.OpeningParagraph.
func OpeningParagraph(body string) string { return mdoutline.OpeningParagraph(body) }

// Anchors is protocol/mdoutline.Anchors.
func Anchors(body string) []string { return mdoutline.Anchors(body) }
