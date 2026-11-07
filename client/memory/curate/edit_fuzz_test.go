package curate

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol/mdoutline"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
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
		"## Plan — later\n\n[p][r] [](#plan--later)\n\n[r]: #plan--later\n",
		"## Plan — later\n\n[m](mark://h/d.md#plan--later) [o](mark://o/d.md#plan--later)\n",
		"## Plan — later\n\n[Plan — later] [Plan — later][] ![Plan — later]\n\n[plan — later]: #plan--later\n",
		"[a — b] [c — d]\n\n[a, b]: /x.md\n[c — d]: /y.md\n",
		"## Plan — later\n\n<mark://h/d.md#plan--later> <https://o/x—y>\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, middle string) {
		body := "# Doc\n\nWhat the doc settles for its readers.\n\n" + middle + "\n\n## Tail — end\n\nsee [t](#tail--end)\n"
		resolver := links.ResolverFor("h")
		in := &Input{Path: "/d.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "doc"}, Links: resolver}
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
		// Every link lands where it did: the document's own on the same
		// heading, any other on the same destination, and none is new.
		was := map[string]int{}
		count := func(doc string, hs []mdoutline.Heading, by int) {
			for _, dest := range followed(doc) {
				path, frag, ok := strings.Cut(dest, "#")
				if self := path == "" || strings.HasPrefix(path, "?") || resolver.Path(in.Path, path) == in.Path; ok && self {
					if i := headingIndex(hs, frag); i >= 0 {
						dest = "heading " + before[i].Anchor
					}
				}
				was[dest] += by
			}
		}
		count(body, before, 1)
		count(out, after, -1)
		for dest, n := range was {
			if n != 0 {
				t.Errorf("links to %q: %+d after the edit", dest, -n)
			}
		}
	})
}

// followed is every link, image and autolink destination in doc, read off the
// parse, so the check shares nothing with the finder curate rebases by.
func followed(doc string) []string {
	src := []byte(doc)
	var out []string
	err := ast.Walk(goldmark.DefaultParser().Parse(text.NewReader(src)), func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch v := n.(type) {
		case *ast.Link:
			if entering {
				out = append(out, string(v.Destination))
			}
		case *ast.Image:
			if entering {
				out = append(out, string(v.Destination))
			}
		case *ast.AutoLink:
			if entering {
				out = append(out, string(v.URL(src)))
			}
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		panic(err) // unreachable: the callback never fails
	}
	return out
}
