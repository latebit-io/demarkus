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
