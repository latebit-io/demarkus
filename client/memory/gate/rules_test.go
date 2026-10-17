package gate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/stylerules"
)

// Every rule in the stylerules table is emitted by the gate under its own
// id, so a rule nobody checks cannot sit in the published guide.
func TestEveryStyleRuleFires(t *testing.T) {
	summary := "A summary sentence long enough to pass the floor.\n\n"
	manyLinks := func(n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "- [doc %d](/d/%d.md): one line\n", i, i)
		}
		return b.String()
	}
	fixtures := map[string]struct{ url, body, description string }{
		stylerules.Frontmatter:              {"/doc.md", "---\nname: doc\n---\n# Title\n\n" + summary, ""},
		stylerules.H1:                       {"/doc.md", "Prose with no heading.\n", ""},
		stylerules.EmDash:                   {"/doc.md", "# Title\n\nA — dash.\n", ""},
		stylerules.DuplicateHeading:         {"/doc.md", "# Title\n\n" + summary + "## A\n\nx\n\n## A\n\ny\n", ""},
		stylerules.HubSize:                  {"/index.md", manyLinks(300), ""},
		stylerules.HubBullet:                {"/index.md", manyLinks(5) + "- [Plan](/p.md): **SHIPPED** 2026-09-01\n", ""},
		stylerules.HubFanout:                {"/index.md", manyLinks(45), ""},
		stylerules.Size:                     {"/doc.md", "# Title\n\n" + summary + strings.Repeat("word ", 2000), ""},
		stylerules.SummaryMissing:           {"/doc.md", "# Title\n\n## Next\n\nx\n", ""},
		stylerules.SummaryRestatesTitle:     {"/doc.md", "# Release Notes\n\nThe release notes.\n", ""},
		stylerules.SummaryPlaceholder:       {"/doc.md", "# Widget\n\nWidget is a thing entity in the system.\n", ""},
		stylerules.HeadingStatus:            {"/doc.md", "# Title\n\n" + summary + "## Phase 1 SHIPPED\n\nx\n", ""},
		stylerules.DescriptionShort:         {"/doc.md", "# Title\n\n" + summary, "too short"},
		stylerules.DescriptionRestatesTitle: {"/doc.md", "# Release Notes\n\n" + summary, "The release notes for the release notes."},
		stylerules.DescriptionPlaceholder:   {"/doc.md", "# Widget\n\n" + summary, "Widget is a thing entity in the system here."},
		stylerules.DescriptionMismatch:      {"/doc.md", "# Title\n\n" + summary, "Something else entirely, long enough to pass the floor."},
	}
	for _, rule := range stylerules.Rules {
		f, ok := fixtures[rule.ID]
		if !ok {
			t.Errorf("rule %q has no fixture; add one or drop the rule from the table", rule.ID)
			continue
		}
		var hit bool
		for _, p := range AuditProblems(f.url, f.body, f.description) {
			id, _, _ := strings.Cut(p, ": ")
			if id == rule.ID {
				hit = true
			}
			if !knownRule(id) {
				t.Errorf("finding %q opens with an id outside the stylerules table", p)
			}
		}
		if !hit {
			t.Errorf("rule %q did not fire on its fixture", rule.ID)
		}
	}
}

func knownRule(id string) bool {
	for _, r := range stylerules.Rules {
		if r.ID == id {
			return true
		}
	}
	return false
}
