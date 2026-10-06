package gate

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mdoutline"
)

func TestShapeProblems(t *testing.T) {
	cases := []struct {
		name string
		url  string
		body string
		want []string // substrings expected; nil means no problems
	}{
		{"summary present", "/plans/x.md", "# Plan\n\nAudience: maintainers. One sentence.\n\n## Goal\n\ntext\n", nil},
		{"heading right after H1", "/plans/x.md", "# Plan\n\n## Goal\n\ntext\n", []string{"no summary under the `# H1`"}},
		{"heading on the next line", "/plans/x.md", "# Plan\n## Goal\n\ntext\n", []string{"no summary"}},
		{"H1 only is a stub", "/plans/x.md", "# Plan\n", nil},
		{"no H1 is the H1 rule's job", "/plans/x.md", "## Goal\n\ntext\n", nil},
		{"index exempt", "/index.md", "# Hub\n\n## Sections\n\n- [A](/a.md): x\n", nil},
		{"log exempt", "/log.md", "# Log\n\n## 2026\n\nx\n", nil},
		{"journal exempt", "/demarkus/journal/2026-09-08.md", "# Journal\n\n## Morning\n\nx\n", nil},
		{"date in heading", "/plans/x.md", "# Plan\n\nSummary.\n\n## Shipped 2026-09-01\n\nx\n", []string{`"Shipped 2026-09-01"`, "anchors"}},
		{"pr number in heading", "/plans/x.md", "# Plan\n\nSummary.\n\n## Merged as PR #428\n\nx\n", []string{`"Merged as PR #428"`}},
		{"status word in heading", "/plans/x.md", "# Plan\n\nSummary.\n\n## Content Addressing COMPLETED\n\nx\n", []string{`"Content Addressing COMPLETED"`}},
		{"lowercase done is a word not a status", "/plans/x.md", "# Plan\n\nSummary.\n\n## What is done first\n\nx\n", nil},
		{"H1 with a date is the name", "/journal/2026-09-08.md", "# Journal 2026-09-08\n\n## Notes\n\nx\n", nil},
		{"three shown then count", "/plans/x.md", "# Plan\n\nSummary.\n\n## A DONE\n\n## B DONE\n\n## C DONE\n\n## D DONE\n", []string{"4 heading(s)", `"A DONE", "B DONE", "C DONE";`}},
		{"kind placeholder summary", "/genres/death-metal.md", "# death metal\n\ndeath metal is a source-reconciled music genre entity in the local music knowledge model.\n\n## Local identity\n\nx\n", []string{"what kind of record", `"is a ... entity"`}},
		{"kind placeholder without a following heading", "/genres/x.md", "# x\n\nx is a record in the catalog.\n", []string{"what kind of record"}},
		{"summary restates the title", "/genres/death-metal.md", "# Death Metal\n\nThe death metal.\n\n## Origin\n\nx\n", []string{"restates the title"}},
		{"real summary passes", "/genres/death-metal.md", "# death metal\n\nAn extreme metal style from mid-1980s Florida and Sweden built on growled vocals, blast beats and down-tuned riffs.\n\n## Origin\n\nx\n", nil},
		{"is a entity in prose later is fine", "/plans/x.md", "# Plan\n\nFor maintainers: what this settles.\n\n## Goal\n\nA hub is a document that links.\n", nil},
		{"only the opening paragraph is the summary", "/x/atlas.md", "# Atlas\n\nThe Atlas.\n\nDetails explain its archival API.\n\n## Uses\n\nx\n", []string{"restates the title"}},
		{"setext H1 underline is not the summary", "/x/atlas.md", "Atlas\n=====\n\nA catalog of archival APIs and the teams that run them.\n\n## Uses\n\nx\n", nil},
		{"crlf paragraphs", "/x/atlas.md", "# Atlas\r\n\r\nThe Atlas.\r\n\r\nDetails explain its archival API.\r\n\r\n## Uses\r\n\r\nx\r\n", []string{"restates the title"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shapeProblems(c.url, c.body, mdoutline.Headings(c.body))
			if c.want == nil {
				if len(got) != 0 {
					t.Fatalf("want no problems, got %q", got)
				}
				return
			}
			joined := strings.Join(got, " | ")
			for _, w := range c.want {
				if !strings.Contains(joined, w) {
					t.Errorf("want %q in problems, got %q", w, got)
				}
			}
		})
	}
}

func TestShapeGateThroughEvaluate(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	d := mustEval(t, styleInput("# Plan\n\n## Phase 1 SHIPPED\n\ntext\n"))
	if d.Decision != "warn" || !strings.Contains(d.Reason, "no summary") || !strings.Contains(d.Reason, "carry status") {
		t.Fatalf("want both shape warnings, got %q (%s)", d.Decision, d.Reason)
	}
	clean := mustEval(t, styleInput("# Plan\n\nFor maintainers: what this settles.\n\n## Phase 1\n\nStatus: shipped\n"))
	if clean.Decision != "allow" {
		t.Fatalf("want allow, got %q (%s)", clean.Decision, clean.Reason)
	}
}

func TestDescriptionProblems(t *testing.T) {
	body := "# death metal\n\nAn extreme metal style from mid-1980s Florida and Sweden built on growled vocals and blast beats.\n\n## Origin\n\nx\n"
	cases := []struct {
		name        string
		body        string
		description string
		want        []string
	}{
		{"absent is the policy's job", body, "", nil},
		{"identical passes", body, "An extreme metal style from mid-1980s Florida and Sweden built on growled vocals and blast beats.", nil},
		{"whitespace differences pass", body, "An extreme metal style  from mid-1980s Florida and Sweden built on growled vocals and blast beats. ", nil},
		{"too short", body, "Genre record.", []string{"2 word(s)", "at least 6"}},
		{"differs from the H1 line", body, "A metal genre that emerged in the mid-1980s in the United States.", []string{"differs from the summary line"}},
		{"kind placeholder", body, "death metal is a source-reconciled music genre entity in the local model.", []string{"what kind of record", "differs from"}},
		{"no H1 compares nothing", "Just prose.\n", "A sentence long enough to pass the word floor easily.", nil},
		{"stub body compares nothing", "# Plan\n", "A sentence long enough to pass the word floor easily.", nil},
		{"setext underline skipped", "Death metal\n===========\n\nAn extreme metal style from mid-1980s Florida and Sweden built on growled vocals and blast beats.\n\n## Origin\n\nx\n", "An extreme metal style from mid-1980s Florida and Sweden built on growled vocals and blast beats.", nil},
		{"crlf body matches", "# Title\r\n\r\nA matching summary sentence with enough words.\r\n\r\nMore detail.\r\n", "A matching summary sentence with enough words.", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := descriptionProblems(c.body, mdoutline.Headings(c.body), c.description)
			if c.want == nil {
				if len(got) != 0 {
					t.Fatalf("want no problems, got %q", got)
				}
				return
			}
			joined := strings.Join(got, " | ")
			for _, w := range c.want {
				if !strings.Contains(joined, w) {
					t.Errorf("want %q in problems, got %q", w, got)
				}
			}
		})
	}
}
