// Package gate: documentation-style guard. Split into its own file: the
// tag/destination/retention gates read only metadata, while this one reads
// the document body against the mechanically checkable rules of the
// knowledge system style guide (mark://root/.well-known/demarkus/style.md).
package gate

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// styleDecision checks a publish body against the style rules that can be
// verified mechanically: no frontmatter fence opening the body, an H1
// present, no em dashes, and unique headings (headings are anchors; a
// duplicate gets a -1 suffix that shifts when sections move, silently
// breaking inbound links). Publish only: appends are fragments, where an H1
// or fence heuristic would misfire. Default severity warn
// (DEMARKUS_STYLE_STRICTNESS overrides): style steers, it does not wall.
func styleDecision(pt config.ParsedTool, args map[string]any) (*Decision, error) {
	if pt.Verb != "publish" {
		return nil, nil
	}
	body, _ := args["body"].(string)
	if strings.TrimSpace(body) == "" {
		return nil, nil
	}
	leaf := urlOf(args)
	if i := strings.LastIndex(leaf, "/"); i >= 0 {
		leaf = leaf[i+1:]
	}

	var problems []string
	if bodyOpensWithFrontmatter(body) {
		problems = append(problems,
			"the body opens with a YAML frontmatter fence; demarkus carries metadata out of band, so the block is stored as literal content (garbled headings, invisible to mark_lookup); move name/tags/type into the metadata object and delete the fence")
	}

	headings := mdoutline.Headings(body)
	// index.md and log.md are OKF navigation/history files, exempt from the
	// H1 rule the same way policy exempts them from `type`.
	if leaf != "index.md" && leaf != "log.md" && !hasH1(headings) {
		problems = append(problems,
			"no `# H1` heading (the document's name is its H1; add one as the first line)")
	}

	if n := strings.Count(body, "—"); n > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d em dash(es); the style guide bans them everywhere (prose, headings, examples); use a comma, colon, semicolon, or parentheses", n))
	}

	if dups := duplicateHeadings(headings); len(dups) > 0 {
		problems = append(problems, fmt.Sprintf(
			"duplicate headings (%s); headings are #section anchors, and duplicates get -1 suffixes that shift when sections move, breaking inbound links; make each heading unique", strings.Join(dups, ", ")))
	}

	if len(problems) == 0 {
		return nil, nil
	}
	target := urlOr(args, "the document")
	reason := fmt.Sprintf(
		"demarkus publish to %s has style problems: %s. See mark://root/.well-known/demarkus/style.md. DEMARKUS_STYLE_STRICTNESS adjusts this check (warn/ask/block).",
		target, strings.Join(problems, " Also: "))
	s, err := config.StyleStrictness()
	if err != nil {
		return nil, err
	}
	return &Decision{Decision: string(s), Reason: reason}, nil
}

// bodyOpensWithFrontmatter reports whether the body starts with a YAML
// frontmatter block: a leading `---` line, a closing `---` line within the
// next 30 lines, and at least one `key: value` line between them. The extra
// conditions keep a document that legitimately opens with a horizontal rule
// from being flagged.
func bodyOpensWithFrontmatter(body string) bool {
	lines := strings.Split(strings.TrimLeft(body, "\n\r\t "), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r ") != "---" {
		return false
	}
	sawKV := false
	limit := min(len(lines), 31)
	for _, ln := range lines[1:limit] {
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

// duplicateHeadings returns the heading texts whose anchor slugs collide.
// mdoutline.Headings excludes # lines inside code fences, so bash comments
// in examples never count.
func duplicateHeadings(headings []mdoutline.Heading) []string {
	counts := make(map[string]int, len(headings))
	for _, h := range headings {
		counts[mdoutline.Slug(h.Text)]++
	}
	var dups []string
	seen := make(map[string]bool)
	for _, h := range headings {
		s := mdoutline.Slug(h.Text)
		if counts[s] > 1 && !seen[s] {
			seen[s] = true
			dups = append(dups, fmt.Sprintf("%q", h.Text))
		}
	}
	return dups
}
