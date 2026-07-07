package gate

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func styleInput(body string) Input {
	return Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
		"url": "/doc.md", "body": body, "metadata": map[string]any{"tags": "a,b"},
	}}
}

func TestStyleGate(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})

	t.Run("clean body allows", func(t *testing.T) {
		d := mustEval(t, styleInput("# Title\n\nOne sentence summary.\n\n## Setup\n\nSteps.\n"))
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("frontmatter fence warns", func(t *testing.T) {
		d := mustEval(t, styleInput("---\nname: doc\ntype: reference\n---\n# Title\n\nBody.\n"))
		if d.Decision != "warn" || !strings.Contains(d.Reason, "frontmatter fence") {
			t.Fatalf("want warn about fence, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("horizontal rule start is not a fence", func(t *testing.T) {
		d := mustEval(t, styleInput("---\n\n# Title\n\nBody after a rule.\n"))
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("missing H1 warns", func(t *testing.T) {
		d := mustEval(t, styleInput("Just prose with no heading at all.\n"))
		if d.Decision != "warn" || !strings.Contains(d.Reason, "H1") {
			t.Fatalf("want warn about H1, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("index.md exempt from H1", func(t *testing.T) {
		d := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
			"url": "/world/index.md", "body": "- [a](a.md)\n", "metadata": map[string]any{"tags": "a"},
		}})
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("em dash warns", func(t *testing.T) {
		d := mustEval(t, styleInput("# Title\n\nA sentence — with an em dash.\n"))
		if d.Decision != "warn" || !strings.Contains(d.Reason, "em dash") {
			t.Fatalf("want warn about em dash, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("duplicate headings warn", func(t *testing.T) {
		d := mustEval(t, styleInput("# Title\n\n## Notes\n\na\n\n## Notes\n\nb\n"))
		if d.Decision != "warn" || !strings.Contains(d.Reason, "duplicate headings") {
			t.Fatalf("want warn about duplicates, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("em dash inside a code fence still warns", func(t *testing.T) {
		// By design: the guide bans em dashes in examples too, and fenced
		// examples are what readers copy.
		d := mustEval(t, styleInput("# Title\n\n```text\nquoted — output\n```\n"))
		if d.Decision != "warn" || !strings.Contains(d.Reason, "em dash") {
			t.Fatalf("want warn about em dash, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("hash comments in code fences are not headings", func(t *testing.T) {
		body := "# Title\n\n```bash\n# comment\n# comment\n```\n\nProse.\n"
		d := mustEval(t, styleInput(body))
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("append is exempt", func(t *testing.T) {
		d := mustEval(t, Input{Tool: "demarkus_memory_mark_append", Input: map[string]any{
			"url": "/doc.md", "body": "fragment — no H1, and that is fine for the style gate\n",
		}})
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("empty body is exempt", func(t *testing.T) {
		d := mustEval(t, styleInput(""))
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("strictness env override", func(t *testing.T) {
		t.Setenv("DEMARKUS_STYLE_STRICTNESS", "block")
		d := mustEval(t, styleInput("No heading — and an em dash.\n"))
		if d.Decision != "block" {
			t.Fatalf("want block, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("tag-gate block outranks style warn", func(t *testing.T) {
		t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "block")
		d := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
			"url": "/doc.md", "body": "no heading here",
		}})
		if d.Decision != "block" || !strings.Contains(d.Reason, "metadata.tags") {
			t.Fatalf("want tag-gate block, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("knowledge surface gets style gate too", func(t *testing.T) {
		setupHome(t, map[string]string{"knowledge-systems": "knowledge\n"})
		d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{
			"url": "/k.md", "body": "prose — no heading", "metadata": map[string]any{"tags": "a,b", "type": "Note"},
		}})
		if d.Decision != "warn" || !strings.Contains(d.Reason, "style") {
			t.Fatalf("want style warn, got %q (%s)", d.Decision, d.Reason)
		}
	})
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
			if got := bodyOpensWithFrontmatter(c.body); got != c.want {
				t.Errorf("bodyOpensWithFrontmatter(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestStyleStrictnessDefault(t *testing.T) {
	setupHome(t, nil)
	s, err := config.StyleStrictness()
	if err != nil {
		t.Fatal(err)
	}
	if s != config.Warn {
		t.Errorf("default style strictness = %q, want warn", s)
	}
}
