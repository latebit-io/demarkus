package curate

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol/mdoutline"
)

// An edit keeps the headings in order, so heading i before is heading i
// after: a link that resolved, or an anchor followed through Moved, must
// still land on the same index.
func FuzzEditKeepsLinks(f *testing.F) {
	for _, seed := range []string{
		"## Twin (DONE)\n\nfirst\n\n## Twin\n\n[t](#twin) [d](#twin-done)\n",
		"## A — B\n\n[e](#a--b) [f](#a—b)\n",
		"## Step 3 #572\n\n[s](#step-3-572 \"t\") \\[x](#step-3-572)\n",
		"```\n## Not (DONE)\n```\n[c](#not-done)\n",
		"## X (DONE)\n\n## X (WIP)\n\n## X\n\n[a](#x) [b](#x-1) [c](#x-wip)\n",
		"Setext (DONE)\n----\n\n[s](#setext-done)\n",
		"- [l](#later)\n\n### Later — maybe (DONE)\n",
		"## Plan — later\n\n[a](/d.md#plan--later) [r](d.md#Plan--later) [o](/o.md#plan--later) [q](?v=1#plan--later)\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, middle string) {
		body := "# Doc\n\nWhat the doc settles for its readers.\n\n" + middle + "\n\n## Tail — end\n\nsee [t](#tail--end)\n"
		in := &Input{Path: "/d.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "doc"}}
		p := Propose(in)
		if p.Kind != KindEdit {
			return
		}
		out := p.Writes[0].Body
		before, after := mdoutline.Headings(body), mdoutline.Headings(out)
		if len(before) != len(after) {
			t.Fatalf("headings %d -> %d", len(before), len(after))
		}
		// An inbound link, repointed through Moved where listed, lands on its heading.
		for i, h := range before {
			to := h.Anchor
			if m, ok := p.Moved[h.Anchor]; ok {
				to = strings.TrimPrefix(m, in.Path+"#")
			}
			if got := headingIndex(after, to); got != i {
				t.Errorf("anchor %q (heading %d %q) lands on %d", h.Anchor, i, h.Text, got)
			}
		}
		// The document's own links, by fragment or by its path, land where they did.
		was := map[int]int{}
		count := func(doc string, hs []mdoutline.Heading, by int) {
			for _, l := range links.FragmentLinks(doc) {
				if i := headingIndex(hs, l.Anchor); i >= 0 {
					was[i] += by
				}
			}
			for _, l := range links.ExtractWithPositions(doc) {
				if self := strings.HasPrefix(l.Dest, "?") || links.ResolvePath(in.Path, l.Dest) == in.Path; l.Fragment != "" && self {
					if i := headingIndex(hs, l.Fragment); i >= 0 {
						was[i] += by
					}
				}
			}
		}
		count(body, before, 1)
		count(out, after, -1)
		for i, n := range was {
			if n > 0 {
				t.Errorf("%d link(s) to heading %d %q no longer resolve to it", n, i, before[i].Text)
			}
		}
	})
}
