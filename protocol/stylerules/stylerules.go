// Package stylerules is the one table of the mechanically checked document
// style rules: the gate prefixes each finding with a rule id, the style
// guide a knowledge system publishes renders the table between markers, and
// the doctor compares the two. A rule changes here or nowhere.
package stylerules

import "strings"

// DocumentPath is where a knowledge system publishes its style guide, the
// document every style finding cites.
const DocumentPath = "/.well-known/demarkus/style.md"

// Rule is one checked rule: a stable id findings carry and one line a
// writer can act on.
type Rule struct {
	ID      string
	Summary string
}

// Rule ids, in guide order.
const (
	Frontmatter              = "frontmatter"
	H1                       = "h1"
	EmDash                   = "em-dash"
	DuplicateHeading         = "duplicate-heading"
	HubSize                  = "hub-size"
	HubBullet                = "hub-bullet"
	HubFanout                = "hub-fanout"
	Size                     = "size"
	SummaryMissing           = "summary-missing"
	SummaryRestatesTitle     = "summary-restates-title"
	SummaryPlaceholder       = "summary-placeholder"
	HeadingStatus            = "heading-status"
	DescriptionShort         = "description-short"
	DescriptionRestatesTitle = "description-restates-title"
	DescriptionPlaceholder   = "description-placeholder"
	DescriptionMismatch      = "description-mismatch"
)

// Rules is the table, in guide order.
var Rules = []Rule{
	{Frontmatter, "metadata travels out of band; a body that opens with a YAML frontmatter fence stores the fence literally"},
	{H1, "every document opens with one `# H1`, its name; `index.md` and `log.md` are exempt"},
	{EmDash, "no em dashes anywhere, prose, headings or examples; use a comma, colon, semicolon or parentheses"},
	{DuplicateHeading, "headings are unique within a document; a duplicate takes a shifting `-1` anchor suffix"},
	{HubSize, "a hub stays under 8 KB so a plain fetch returns the whole page"},
	{HubBullet, "a hub bullet is a link plus one line; no bold, `Status:`, dates or PR numbers, which belong in the child"},
	{HubFanout, "a hub links about 40 documents; past that, add a second-level hub"},
	{Size, "a document other than a journal stays under 8 KB; past that, split it into a hub plus topic files"},
	{SummaryMissing, "one summary sentence sits directly under the `# H1`; hubs and journals are exempt"},
	{SummaryRestatesTitle, "the summary says what the subject is, not the title again"},
	{SummaryPlaceholder, "the summary describes the subject, not the kind of record (\"is a ... entity\")"},
	{HeadingStatus, "a heading below the H1 is a name alone; status (a date, a PR number, DONE, SHIPPED) goes on the first line under it"},
	{DescriptionShort, "`metadata.description` is a sentence of at least six words"},
	{DescriptionRestatesTitle, "`metadata.description` says what the subject is, not the title again"},
	{DescriptionPlaceholder, "`metadata.description` describes the subject, not the kind of record"},
	{DescriptionMismatch, "`metadata.description` equals the summary line under the H1; one sentence in two slots"},
}

// Markers fence the rendered table inside a published style guide so the
// prose around it stays hand-written.
const (
	BeginMarker = "<!-- stylerules:begin -->"
	EndMarker   = "<!-- stylerules:end -->"
)

// Markdown renders the table as the section a style guide carries between
// the markers, markers included.
func Markdown() string {
	var b strings.Builder
	b.WriteString(BeginMarker + "\n")
	b.WriteString("These rules are checked by the style gate on every publish and by the doctor over the catalog. A finding names its rule id.\n\n")
	for _, r := range Rules {
		b.WriteString("- `" + r.ID + "`: " + r.Summary + "\n")
	}
	b.WriteString(EndMarker)
	return b.String()
}

// Current reports whether a published guide carries the table this build
// renders; a guide without the markers is not current.
func Current(body string) bool { return strings.Contains(body, Markdown()) }

// Prefix words a finding as the guide lists it: the id, a colon, the text.
func Prefix(id, finding string) string { return id + ": " + finding }
