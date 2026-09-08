package gate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
)

// Document shape rules from the style guide: a summary sentence under the
// H1, and headings that are stable anchors (no status in them).
var (
	headingStatusWord = regexp.MustCompile(`\b(COMPLETE|COMPLETED|SHIPPED|DONE|MERGED|CLOSED|WIP)\b`)
)

// shapeProblems checks the summary and heading rules. Hubs and journals are
// exempt from the summary rule: navigation has no summary, a journal's is
// its date.
func shapeProblems(url, body string, headings []mdoutline.Heading) []string {
	var problems []string
	leaf := leafOf(url)
	if !navExempt(leaf) && !journalPath(url) && missingSummary(body, headings) {
		problems = append(problems,
			"no summary under the `# H1` (the next line is a heading); add one sentence saying who the document is for and what it settles")
	}
	var status []string
	for _, h := range headings {
		if h.Level == 1 {
			continue
		}
		if hubISODate.MatchString(h.Text) || hubPRNumber.MatchString(h.Text) || headingStatusWord.MatchString(h.Text) {
			status = append(status, fmt.Sprintf("%q", h.Text))
		}
	}
	if len(status) > 0 {
		shown := status
		if len(shown) > 3 {
			shown = shown[:3]
		}
		problems = append(problems, fmt.Sprintf(
			"%d heading(s) carry status (a date, a PR number, COMPLETE, SHIPPED, DONE): %s; headings are anchors that inbound links depend on, so keep the name alone and put status on the first line under it",
			len(status), strings.Join(shown, ", ")))
	}
	return problems
}

func journalPath(url string) bool {
	return slices.Contains(strings.Split(url, "/"), "journal")
}

// missingSummary: an H1 exists and the text between it and the next heading
// is blank. A document with an H1 and nothing after it is a stub, not a
// missing summary.
func missingSummary(body string, headings []mdoutline.Heading) bool {
	for i, h := range headings {
		if h.Level != 1 {
			continue
		}
		if i+1 >= len(headings) {
			return false
		}
		between := body[h.Start:headings[i+1].Start]
		if nl := strings.IndexByte(between, '\n'); nl >= 0 {
			between = between[nl+1:]
		} else {
			between = ""
		}
		return strings.TrimSpace(between) == ""
	}
	return false
}
