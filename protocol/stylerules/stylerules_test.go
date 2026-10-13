package stylerules

import (
	"strings"
	"testing"
)

func TestRulesAreUniqueAndRendered(t *testing.T) {
	seen := map[string]bool{}
	md := Markdown()
	for _, r := range Rules {
		if seen[r.ID] {
			t.Errorf("duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		if r.ID == "" || r.Summary == "" {
			t.Errorf("rule %+v is incomplete", r)
		}
		if !strings.Contains(md, "- `"+r.ID+"`: ") {
			t.Errorf("rule %q not rendered", r.ID)
		}
		if strings.Contains(r.Summary, "—") {
			t.Errorf("rule %q carries an em dash", r.ID)
		}
	}
}

func TestCurrent(t *testing.T) {
	guide := "# Style\n\nProse above.\n\n" + Markdown() + "\n\nProse below.\n"
	if !Current(guide) {
		t.Error("a guide carrying the rendered table is not current")
	}
	stale := strings.Replace(guide, "- `"+EmDash+"`", "- `em-dashes`", 1)
	if Current(stale) {
		t.Error("a guide with a renamed rule reads current")
	}
	if Current("# Style\n\nno markers\n") {
		t.Error("a guide without markers reads current")
	}
}
