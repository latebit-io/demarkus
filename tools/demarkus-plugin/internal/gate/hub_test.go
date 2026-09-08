package gate

import (
	"fmt"
	"strings"
	"testing"
)

// linkBullets renders n one-line link bullets.
func linkBullets(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "- [Doc %d](/docs/d%d.md): one line about it\n", i, i)
	}
	return b.String()
}

func TestHubProblems(t *testing.T) {
	cases := []struct {
		name string
		leaf string
		body string
		want []string // substrings each expected in one problem; nil means no problems
	}{
		{"clean index", "index.md", "# Hub\n\nOne line.\n\n## Sections\n\n" + linkBullets(6), nil},
		{"index over the fetch threshold", "index.md",
			"# Hub\n\n" + linkBullets(6) + strings.Repeat("prose line\n", 900), []string{"KB, at or over the 8 KB"}},
		{"long bullet names its link", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Long](/l.md): " + strings.Repeat("word ", 50) + "\n", []string{`"Long"`, "one line"}},
		{"long destination does not count", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Short](/" + strings.Repeat("p/", 120) + "x.md): fine\n", nil},
		{"date outside the link is status", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Plan](/p.md): shipped 2026-09-01\n", []string{`"Plan"`, "status"}},
		{"pr number is status", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Plan](/p.md): merged as PR #428\n", []string{`"Plan"`}},
		{"bold is status", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Plan](/p.md): **COMPLETE** and deployed\n", []string{`"Plan"`}},
		{"status key is status", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Plan](/p.md): Status: active\n", []string{`"Plan"`}},
		{"date inside link text passes", "index.md",
			"# Journal\n\n- [2026-09-08](/journal/2026-09-08.md): ranking\n- [2026-09-07](/journal/2026-09-07.md): body match\n", nil},
		{"marker inside a code span passes", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Format](/roadmap.md#format): `Status: <active|done>` is the first line under each entry\n", nil},
		{"anchor in destination is not a pr number", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Format](/roadmap.md#1-format): the rule\n", nil},
		{"hard wrapped bullet is one line", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Wrapped](/w.md): first half of the line\n  second half of the line\n", nil},
		{"second paragraph in a bullet fails", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Two](/t.md): first paragraph\n\n  second paragraph\n", []string{`"Two"`}},
		{"nested list is not a second paragraph", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Topic](/t.md): the file\n  - [Entry](/t.md#entry)\n", nil},
		{"bullet without a link is labelled by its words", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- **soul**: personal store, direct QUIC\n", []string{`"soul: personal store, direct QUIC"`}},
		{"over forty outbound documents", "index.md",
			"# Hub\n\n" + linkBullets(41), []string{"41 outbound documents", "second-level hub"}},
		{"anchored links into one document count once", "index.md",
			"# Hub\n\n" + linkBullets(38) + "- [A](/a.md#x)\n- [B](/a.md#y)\n- [C](/a.md#z)\n- [D](https://example.com/d)\n", nil},
		{"reference links make a link page", "related.md",
			"# Related\n\n" + strings.Repeat("- [Doc][d]: one line\n", 6) + "- [Plan][p]: merged 2026-09-01\n\n[d]: /docs/d.md\n[p]: /p.md\n", []string{`"Plan"`}},
		{"collapsed reference link with a long destination passes", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Short][]: fine\n\n[Short]: /" + strings.Repeat("p/", 120) + "x.md\n", nil},
		{"resolved shortcut reference with a date passes", "index.md",
			"# Journal\n\n- [2026-09-08]: ranking\n- [2026-09-07]: body match\n- [2026-09-06]: x\n- [2026-09-05]: y\n- [2026-09-04]: z\n\n[2026-09-08]: /journal/2026-09-08.md\n[2026-09-07]: /journal/2026-09-07.md\n[2026-09-06]: /j6.md\n[2026-09-05]: /j5.md\n[2026-09-04]: /j4.md\n", nil},
		{"unresolved reference is text and carries status", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [Plan](/p.md) [Status: active][missing]\n", []string{`"Plan"`}},
		{"autolink bullets make a link page", "links.md",
			"# Links\n\n- <https://a.example/1>: one\n- <https://a.example/2>: two\n- <https://a.example/3>: three\n- <https://a.example/4>: four\n- <https://a.example/5>: five\n- [Plan](/p.md): merged 2026-09-01\n", []string{`"Plan"`}},
		{"emphasis-wrapped links make a link page", "wrapped.md",
			"# Wrapped\n\n- **[A](/a.md)**: a\n- *[B](/b.md)*: b\n- **[C](/c.md)**: c\n- **[D](/d.md)**: d\n- ***[E](/e.md)***: e\n- [Plan](/p.md): merged 2026-09-01\n", []string{`"Plan"`}},
		{"bold link label is a link, not status", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- **[Plan](/p.md)**: the plan\n", nil},
		{"link page by shape gets the rules", "related.md",
			"# Related\n\nLinks:\n\n" + linkBullets(8) + "- [Plan](/p.md): merged 2026-09-01\n", []string{`"Plan"`}},
		{"prose document with a short list is not a hub", "notes.md",
			"# Notes\n\n" + strings.Repeat("A prose line.\n", 10) + "\n- [Plan](/p.md): merged 2026-09-01\n- [B](/b.md): x\n- [C](/c.md): y\n- [D](/d.md): z\n- [E](/e.md): w\n", nil},
		{"code fence bullets are ignored", "index.md",
			"# Hub\n\n" + linkBullets(5) + "```text\n- [X](/x.md): merged 2026-09-01\n```\n", nil},
		{"three offenders shown then count", "index.md",
			"# Hub\n\n" + linkBullets(5) + "- [P1](/1.md): 2026-01-01\n- [P2](/2.md): 2026-01-01\n- [P3](/3.md): 2026-01-01\n- [P4](/4.md): 2026-01-01\n",
			[]string{"4 hub bullet(s)", `"P1", "P2", "P3";`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hubProblems(c.leaf, c.body)
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

func TestHubGateThroughEvaluate(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	body := "# Hub\n\n" + linkBullets(5) + "- [Plan](/p.md): **SHIPPED** PR #12 on 2026-09-01\n"
	d := mustEval(t, &Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
		"url": "/demarkus/index.md", "body": body, "metadata": map[string]any{"tags": "index,hub"},
	}})
	if d.Decision != "warn" || !strings.Contains(d.Reason, "hub bullet") || !strings.Contains(d.Reason, "one line each") {
		t.Fatalf("want hub warn naming the fix, got %q (%s)", d.Decision, d.Reason)
	}

	t.Run("append is exempt", func(t *testing.T) {
		d := mustEval(t, &Input{Tool: "demarkus_memory_mark_append", Input: map[string]any{
			"url": "/demarkus/index.md", "body": "- [Plan](/p.md): **SHIPPED** 2026-09-01\n",
		}})
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("plain document keeps its bullets", func(t *testing.T) {
		d := mustEval(t, styleInput("# Plan\n\nA design.\n\n## Status\n\n- **SHIPPED** 2026-09-01 as PR #12, a long status line that a hub could not carry\n"))
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})
}
