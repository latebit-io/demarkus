// Package gate holds the mechanically checkable document style rules every
// surface applies to a publish: the write path of the MCP tools, the plugin
// hook and the catalog audit. Rules are the knowledge system style guide
// (mark://root/.well-known/demarkus/style.md); strictness is the caller's.
package gate

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/internal/okf"
	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// What a finding cites: the guide a knowledge system publishes, or the
// baseline for a surface without one.
const (
	Baseline       = "Rules: metadata out of band, one # H1 with a summary under it, unique status-free headings, no em dashes, hubs are links plus one line each under 8 KB."
	KnowledgeGuide = "See mark://root/.well-known/demarkus/style.md."
)

// Problems applies every rule to a publish of body at url: frontmatter fence,
// H1, em dashes, unique headings, then the hub, shape and description rules.
// Nil when the body is empty or clean. Appends are fragments: not checked.
func Problems(url, body, description string) []string {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	var problems []string
	if OpensWithFrontmatter(body) {
		problems = append(problems,
			"the body opens with a YAML frontmatter fence; demarkus carries metadata out of band, so the block is stored as literal content (garbled headings, invisible to mark_lookup); move name/tags/type into the metadata object and delete the fence")
	}

	headings := mdoutline.Headings(body)
	if !NavExempt(url) && !hasH1(headings) {
		problems = append(problems,
			"no `# H1` heading (the document's name is its H1; add one as the first line)")
	}

	// Counts the raw body, code fences included: the style guide bans em
	// dashes in examples too, and fenced examples are what readers copy.
	if n := EmDashCount(body); n > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d em dash(es); the style guide bans them everywhere (prose, headings, examples); use a comma, colon, semicolon, or parentheses", n))
	}

	if dups := DuplicateHeadings(headings); len(dups) > 0 {
		problems = append(problems, fmt.Sprintf(
			"duplicate headings (%s); headings are #section anchors, and duplicates get -1 suffixes that shift when sections move, breaking inbound links; make each heading unique", strings.Join(dups, ", ")))
	}

	// A hub at or over the threshold is worded by the hub rules.
	if hub := HubProblems(url, body); len(hub) > 0 {
		problems = append(problems, hub...)
	} else {
		problems = append(problems, sizeProblems(url, body)...)
	}
	problems = append(problems, ShapeProblems(url, body, headings)...)
	problems = append(problems, DescriptionProblems(body, headings, description)...)
	return problems
}

// Findings words problems as one sentence ending in the guide to cite.
func Findings(problems []string, guide string) string {
	return strings.Join(problems, " Also: ") + ". " + guide
}

// sizeProblems applies the fetch threshold to a document that is not a
// journal, which grows by the day and is read by section.
func sizeProblems(url, body string) []string {
	if len(body) < mdoutline.OutlineThreshold || journalPath(url) {
		return nil
	}
	return []string{fmt.Sprintf(
		"body is %d KB, at or over the 8 KB fetch threshold, so a plain fetch returns an outline; split it into a hub plus topic files", len(body)/1024)}
}

// Frontmatter reads the fence OpensWithFrontmatter detects: its keys as the
// OKF parser reads them and the body after the fence.
func Frontmatter(body string) (kv map[string]string, rest string, ok bool) {
	if !OpensWithFrontmatter(body) {
		return nil, body, false
	}
	fm, after, found := okf.SplitFrontmatter([]byte(strings.TrimLeft(body, "\n\r\t ")))
	if !found {
		return nil, body, false
	}
	kv, err := okf.ParseFrontmatter(fm)
	if err != nil {
		return nil, body, false
	}
	return kv, strings.TrimLeft(string(after), "\n"), true
}

// NavExempt reports an OKF navigation/history file (index.md, log.md): exempt
// from the H1 and summary rules as the policy exempts it from `type`.
func NavExempt(url string) bool { return publishpolicy.NavigationExempt(url) }

// EmDashCount counts em dashes in the raw body, code fences included, as
// the style rule does.
func EmDashCount(body string) int { return strings.Count(body, "—") }

// OpensWithFrontmatter reports a body that starts with a YAML frontmatter
// block: a leading `---` line, a closing one within 30 lines, and a
// `key: value` line between, so a document opening with a rule is not flagged.
func OpensWithFrontmatter(body string) bool {
	body = strings.TrimLeft(body, "\n\r\t ")
	if !strings.HasPrefix(body, "---") {
		return false
	}
	lines := strings.SplitN(body, "\n", 32)
	if strings.TrimRight(lines[0], "\r ") != "---" {
		return false
	}
	sawKV := false
	for _, ln := range lines[1:min(len(lines), 31)] {
		ln = strings.TrimRight(ln, "\r ")
		if ln == "---" {
			return sawKV
		}
		key, _, ok := strings.Cut(ln, ":")
		if ok && key != "" && !strings.ContainsAny(key, " \t") {
			sawKV = true
		}
	}
	return false
}

func hasH1(headings []mdoutline.Heading) bool {
	for _, h := range headings {
		if h.Level == 1 {
			return true
		}
	}
	return false
}

// DuplicateHeadings returns the heading texts whose anchor slugs collide.
// mdoutline.Headings excludes # lines inside code fences, so bash comments
// in examples never count.
func DuplicateHeadings(headings []mdoutline.Heading) []string {
	slugs := make([]string, len(headings))
	counts := make(map[string]int, len(headings))
	for i, h := range headings {
		slugs[i] = mdoutline.Slug(h.Text)
		counts[slugs[i]]++
	}
	var dups []string
	seen := make(map[string]bool)
	for i, h := range headings {
		if counts[slugs[i]] > 1 && !seen[slugs[i]] {
			seen[slugs[i]] = true
			dups = append(dups, fmt.Sprintf("%q", h.Text))
		}
	}
	return dups
}
