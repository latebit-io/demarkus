package gate

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func styleInput(body string) *Input {
	return &Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
		"url": "/doc.md", "body": body, "metadata": map[string]any{"tags": "a,b"},
	}}
}

// The rules are tested in client/docwrite/gate; here only how the hook
// applies them: which verbs, at what strictness, on which surfaces.
func TestStyleGate(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})

	t.Run("clean body allows", func(t *testing.T) {
		d := mustEval(t, styleInput("# Title\n\nOne sentence summary.\n\n## Setup\n\nSteps.\n"))
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("flawed body warns and names the fix", func(t *testing.T) {
		d := mustEval(t, styleInput("No heading — and an em dash.\n"))
		if d.Decision != "warn" || !strings.Contains(d.Reason, "H1") || !strings.Contains(d.Reason, "Also: em-dash: 1 em dash") || !strings.Contains(d.Reason, "DEMARKUS_STYLE_STRICTNESS") {
			t.Fatalf("want warn naming both problems, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("append is exempt", func(t *testing.T) {
		d := mustEval(t, &Input{Tool: "demarkus_memory_mark_append", Input: map[string]any{
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
		d := mustEval(t, &Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
			"url": "/doc.md", "body": "no heading here",
		}})
		if d.Decision != "block" || !strings.Contains(d.Reason, "metadata.tags") {
			t.Fatalf("want tag-gate block, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("knowledge surface gets style gate too", func(t *testing.T) {
		setupHome(t, map[string]string{"knowledge-systems": "knowledge\n"})
		d := mustEval(t, &Input{Tool: "knowledge_mark_publish", Input: map[string]any{
			"url": "/k.md", "body": "prose — no heading", "metadata": map[string]any{"tags": "a,b", "type": "Note"},
		}})
		if d.Decision != "warn" || !strings.Contains(d.Reason, "style.md") {
			t.Fatalf("want style warn citing the guide, got %q (%s)", d.Decision, d.Reason)
		}
	})
}

// metadata.description reaches the rules from the hook payload.
func TestStyleGateDescription(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	body := "# Title\n\nA summary sentence long enough to pass the floor.\n\n## Setup\n\nSteps.\n"
	d := mustEval(t, &Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
		"url": "/doc.md", "body": body, "metadata": map[string]any{"tags": "a,b", "description": "Something else entirely, long enough to pass the floor."},
	}})
	if d.Decision != "warn" || !strings.Contains(d.Reason, "differs from the summary line") {
		t.Fatalf("want warn about the mismatch, got %q (%s)", d.Decision, d.Reason)
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
