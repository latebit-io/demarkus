package gate

import (
	"strings"
	"testing"
)

func TestProblems(t *testing.T) {
	cases := []struct {
		name string
		url  string
		body string
		want []string // substrings each expected in one problem; nil means none
	}{
		{"clean body", "/doc.md", "# Title\n\nOne sentence summary.\n\n## Setup\n\nSteps.\n", nil},
		{"empty body is exempt", "/doc.md", "  \n", nil},
		{"frontmatter fence", "/doc.md", "---\nname: doc\ntype: reference\n---\n# Title\n\nBody.\n", []string{"frontmatter fence"}},
		{"horizontal rule start is not a fence", "/doc.md", "---\n\n# Title\n\nBody after a rule.\n", nil},
		{"missing H1", "/doc.md", "Just prose with no heading at all.\n", []string{"H1"}},
		{"index.md exempt from H1", "/world/index.md", "- [a](a.md)\n", nil},
		{"em dash", "/doc.md", "# Title\n\nA sentence — with an em dash.\n", []string{"em dash"}},
		{"em dash inside a code fence", "/doc.md", "# Title\n\n```text\nquoted — output\n```\n", []string{"em dash"}},
		{"duplicate headings", "/doc.md", "# Title\n\n## Notes\n\na\n\n## Notes\n\nb\n", []string{"duplicate headings"}},
		{"hash comments in code fences are not headings", "/doc.md", "# Title\n\n```bash\n# comment\n# comment\n```\n\nProse.\n", nil},
		{"hub bullet with status", "/demarkus/index.md", "# Hub\n\n" + linkBullets(5) + "- [Plan](/p.md): **SHIPPED** PR #12 on 2026-09-01\n", []string{"hub bullet", "one line each"}},
		{"shape: no summary and status heading", "/doc.md", "# Plan\n\n## Phase 1 SHIPPED\n\ntext\n", []string{"no summary", "carry status"}},
		{"plain document keeps its bullets", "/doc.md", "# Plan\n\nA design.\n\n## Status\n\n- **SHIPPED** 2026-09-01 as PR #12, a long status line that a hub could not carry\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Problems(c.url, c.body, "")
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

func TestProblemsDescription(t *testing.T) {
	body := "# Title\n\nA summary sentence long enough to pass the floor.\n\n## Setup\n\nSteps.\n"
	if got := Problems("/doc.md", body, "A summary sentence long enough to pass the floor."); len(got) != 0 {
		t.Fatalf("want no problems, got %q", got)
	}
	got := Problems("/doc.md", body, "Something else entirely, long enough to pass the floor.")
	if len(got) != 1 || !strings.Contains(got[0], "differs from the summary line") {
		t.Fatalf("want the mismatch, got %q", got)
	}
}

func TestBodyOpensWithFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"classic block", "---\nname: x\n---\nbody", true},
		{"crlf block", "---\r\ntype: Note\r\n---\r\nbody", true},
		{"leading blank lines", "\n\n---\ntags: a\n---\n", true},
		{"horizontal rule only", "---\n\ntext\n", false},
		{"rule then heading", "---\n# Title\n---\n", false},
		{"no opening fence", "# Title\n---\nkey: v\n---\n", false},
		{"unclosed fence", "---\nkey: v\nno close", false},
		{"kv with spaces in key is prose", "---\nthis is: prose\n---\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OpensWithFrontmatter(c.body); got != c.want {
				t.Errorf("OpensWithFrontmatter(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}
