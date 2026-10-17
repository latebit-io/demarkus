package gate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/protocol/mdoutline"
	"github.com/latebit-io/demarkus/protocol/stylerules"
)

// Document shape rules from the style guide: a summary sentence under the
// H1 that says what the subject is, and headings that are stable anchors.
var (
	headingStatusWord = regexp.MustCompile(`\b(COMPLETE|COMPLETED|SHIPPED|DONE|MERGED|CLOSED|WIP)\b`)
	// "<name> is a <adjectives> entity in ..." names the record's kind, not
	// its subject: the shape an importer emits when it has no description.
	kindPlaceholder = regexp.MustCompile(`(?i)\bis\s+an?\s+(?:[\w-]+\s+){0,6}?(entity|record|document|page|item|node|stub|placeholder|entry)\b`)
	wordRe          = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}'-]*`)
	spaceRe         = regexp.MustCompile(`\s+`)
	blankLine       = regexp.MustCompile(`\n[ \t]*\n`)
	setextUnderline = regexp.MustCompile(`^[=-]+\s*$`)
)

// StatusTokens are what makes a heading carry status: a date, a PR number,
// a status word. The curator strips them; the shape rule flags them.
var StatusTokens = []*regexp.Regexp{hubISODate, hubPRNumber, headingStatusWord}

// minDescriptionWords is the floor for metadata.description: it is the
// catalog summary, so it must say something without the body.
const minDescriptionWords = 6

// ShapeProblems checks the summary and heading rules. Hubs and journals are
// exempt from the summary rules: navigation has no summary, a journal's is
// its date.
func ShapeProblems(url, body string, headings []mdoutline.Heading) []string {
	var problems []string
	if !NavExempt(url) && !journalPath(url) {
		title, summary, ok := summaryUnderH1(body, headings)
		switch {
		case MissingSummary(body, headings):
			problems = append(problems, stylerules.Prefix(stylerules.SummaryMissing,
				"no summary under the `# H1` (the next line is a heading); add one sentence saying who the document is for and what it settles"))
		case ok:
			problems = append(problems, summaryProblems(summaryRules, "the summary under the `# H1`", title, firstParagraph(summary))...)
		}
	}
	var status []string
	for _, h := range headings {
		if h.Level == 1 {
			continue
		}
		if CarriesStatus(h.Text) {
			status = append(status, fmt.Sprintf("%q", h.Text))
		}
	}
	if len(status) > 0 {
		shown := status
		if len(shown) > 3 {
			shown = shown[:3]
		}
		problems = append(problems, stylerules.Prefix(stylerules.HeadingStatus, fmt.Sprintf(
			"%d heading(s) carry status (a date, a PR number, COMPLETE, SHIPPED, DONE): %s; headings are anchors that inbound links depend on, so keep the name alone and put status on the first line under it",
			len(status), strings.Join(shown, ", "))))
	}
	return problems
}

// CarriesStatus reports a heading with a status token in it.
func CarriesStatus(text string) bool {
	for _, re := range StatusTokens {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// MissingSummary reports an H1 followed by a heading with nothing between.
// An H1 with nothing after it at all is a stub, not a missing summary.
func MissingSummary(body string, headings []mdoutline.Heading) bool {
	_, summary, ok := summaryUnderH1(body, headings)
	return ok && strings.TrimSpace(summary) == "" && headingFollowsH1(headings)
}

// DescriptionProblems checks metadata.description: a word floor, the summary
// rules, and identity with the H1 summary line so neither copy drifts.
// Empty is not a problem here; presence is the publish policy's job.
func DescriptionProblems(body string, headings []mdoutline.Heading, description string) []string {
	description = strings.TrimSpace(description)
	if description == "" {
		return nil
	}
	if n := len(words(description)); n < minDescriptionWords {
		return []string{stylerules.Prefix(stylerules.DescriptionShort, fmt.Sprintf(
			"metadata.description is %d word(s); it is the catalog summary, so write one sentence saying what the subject is (at least %d words)", n, minDescriptionWords))}
	}
	title, summary, ok := summaryUnderH1(body, headings)
	problems := summaryProblems(descriptionRules, "metadata.description", title, description)
	if ok && firstParagraph(summary) != "" && Collapse(firstParagraph(summary)) != Collapse(description) {
		problems = append(problems, stylerules.Prefix(stylerules.DescriptionMismatch,
			"metadata.description differs from the summary line under the `# H1`; they are one sentence in two slots (the catalog stores the metadata copy, readers see the body copy), so make them identical"))
	}
	return problems
}

// summaryRuleIDs names the two summary rules for one slot, so the body
// summary and metadata.description report under their own ids.
type summaryRuleIDs struct{ restates, placeholder string }

var (
	summaryRules     = summaryRuleIDs{stylerules.SummaryRestatesTitle, stylerules.SummaryPlaceholder}
	descriptionRules = summaryRuleIDs{stylerules.DescriptionRestatesTitle, stylerules.DescriptionPlaceholder}
)

// summaryProblems: a summary that restates the title or names only the
// record's kind tells a reader nothing the path did not.
func summaryProblems(ids summaryRuleIDs, what, title, summary string) []string {
	var problems []string
	if restatesTitle(title, summary) {
		problems = append(problems, stylerules.Prefix(ids.restates, what+" restates the title; say what the subject is, where it comes from, and what sets it apart"))
	}
	if m := kindPlaceholder.FindStringSubmatch(summary); m != nil {
		problems = append(problems, stylerules.Prefix(ids.placeholder, fmt.Sprintf(
			"%s says what kind of record this is (\"is a ... %s\") instead of describing the subject; a reader learns nothing from it, so describe the subject itself with sourced facts", what, m[1])))
	}
	return problems
}

// summaryUnderH1 returns the H1 text and the text between it and the next
// heading (or the end of the body). ok is false without an H1.
func summaryUnderH1(body string, headings []mdoutline.Heading) (title, summary string, ok bool) {
	for i, h := range headings {
		if h.Level != 1 {
			continue
		}
		end := len(body)
		if i+1 < len(headings) {
			end = headings[i+1].Start
		}
		return h.Text, afterHeading(body[h.Start:end]), true
	}
	return "", "", false
}

// afterHeading drops the heading's own lines: the text line, plus a setext
// underline (=== or ---) when one follows it.
func afterHeading(section string) string {
	_, rest, found := strings.Cut(section, "\n")
	if !found {
		return ""
	}
	line, after, _ := strings.Cut(rest, "\n")
	if setextUnderline.MatchString(strings.TrimRight(line, "\r")) {
		return after
	}
	return rest
}

// headingFollowsH1 reports a heading after the first H1.
func headingFollowsH1(headings []mdoutline.Heading) bool {
	for i, h := range headings {
		if h.Level == 1 {
			return i+1 < len(headings)
		}
	}
	return false
}

// firstParagraph is the text up to the first blank line; a line of spaces
// or tabs is blank, as in CommonMark, and CRLF counts as a line end.
func firstParagraph(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if loc := blankLine.FindStringIndex(s); loc != nil {
		s = s[:loc[0]]
	}
	return s
}

// Collapse normalizes whitespace so a wrapped body line equals its
// single-line metadata copy.
func Collapse(s string) string {
	return spaceRe.ReplaceAllString(strings.TrimSpace(s), " ")
}

// restatesTitle: every word of the summary is a title word or a stopword,
// and at least one title word is present.
func restatesTitle(title, summary string) bool {
	titleWords := words(title)
	if len(titleWords) == 0 {
		return false
	}
	shared := false
	for _, w := range words(summary) {
		if slices.Contains(titleWords, w) {
			shared = true
			continue
		}
		if !stopword[w] {
			return false
		}
	}
	return shared
}

// words lowercases s and splits it into word tokens.
func words(s string) []string {
	return wordRe.FindAllString(strings.ToLower(s), -1)
}

var stopword = map[string]bool{
	"a": true, "an": true, "the": true, "this": true, "that": true, "is": true, "are": true,
	"was": true, "of": true, "for": true, "and": true, "or": true, "in": true, "on": true,
	"to": true, "it": true, "its": true, "about": true, "page": true, "document": true,
}

// journalPath reports a path with a journal segment, the memory template's
// convention for dated entries.
func journalPath(url string) bool {
	return slices.Contains(strings.Split(url, "/"), "journal")
}
