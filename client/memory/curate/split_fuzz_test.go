package curate

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/outline"
	"github.com/latebit-io/demarkus/protocol/mdoutline"
)

// Whatever Markdown a document holds, a split refuses or keeps every heading
// and every link that resolved, and repoints only to the same heading.
// Checked from the writes alone, not from how the split computed them.
func FuzzSplitKeepsHeadingsAndLinks(f *testing.F) {
	for _, seed := range []string{
		"## Mid\n\nsee [n](#next-1), [p](#pad-a \"t\"), \\[e](#pad-a)\n\n### Next\n\nmid next\n",
		"- a\n  - b\n\n    ## Nested\n\n    see [i](#next)\n",
		"# Second\n\ntext [x](#pad-b)\n",
		"```\n## not a heading\n```\n[c](#PAD-A) [d](#pad.a)\n",
		"## Setext\n\nSetext two\n----------\n\n[s](#setext-two)\n",
		"<div>\n\n## In html\n\n</div>\n",
		"[r][ref]\n\n[ref]: #next\n",
		"### Next\n\nfirst\n\n### Next\n\nsecond\n\n[a](#next) [b](#next-1) [c](#next-2)\n",
		"## Topics\n\n[t](#topics) [u](/d.md#pad-b)\n",
		"## Mid\n\nx\n\n# Second\n\ndropped unless refused\n",
		"- a\n  - b\n\n    ## Nested big\n\n    " + strings.Repeat("nested text ", 100) + "\n",
		"## Mid\n\n[r](rel.md?q#f) ![i](img.png) [d][dm] [s](d.md#pad-b) [e]() <mark://x/y> [p][pa]\n\n[dm]: ../doc.md \"T\"\n",
		"## Mid\n\n[c][PA] [m][mid]\n\n" + strings.Repeat("mid text ", 300) + "\n\n[mid]: #next\n",
		"## Mid\n\n[z][qd] " + strings.Repeat("mid text ", 300) + "\n\n> [qd]: <q d.md>\n> 'T'\n",
		// Fuzz finds: twins of the harness's Next heading, a link to the topics list.
		"- A\n  - A\n#     neXt \n ## 00000000000000",
		"## NeXt",
		"[](#topics )",
	} {
		f.Add(seed)
	}
	filler := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod.\n", 60)
	f.Fuzz(func(t *testing.T, middle string) {
		body := "# Doc\n\nWhat the doc settles; see [a](#pad-a).\n\n" + middle +
			"\n\n## Pad A\n\nup to [u](../up.md).\n\n### Next\n\npad next\n\n[pa]: other.md\n\n" + filler +
			"\n## Pad B\n\nsee [n](#next) and [m](#Pad-A), [o][pa].\n\n" + filler
		in := &Input{Path: "/d.md", Body: body, Version: 1, Metadata: map[string]string{"tags": "doc"}}
		if p := Propose(in); p.Kind == KindSplit {
			for _, v := range splitViolations(in, &p) {
				t.Error(v)
			}
		}
	})
}

var (
	linkDest = regexp.MustCompile(`\]\([^)]*\)`)
	// refDef is a reference definition line: not text, and a split may add one.
	refDef = regexp.MustCompile(`^\[[^\]]*\]:`)
)

// signature names heading i of body by its text and the first line under it
// before any other heading, links blanked: a twin "Next" elsewhere differs, a
// rebased link does not.
func signature(body string, hs []mdoutline.Heading, i int) string {
	h, end := hs[i], len(body)
	if i+1 < len(hs) {
		end = hs[i+1].Start
	}
	for _, line := range strings.Split(body[h.Start:end], "\n")[1:] {
		line = strings.TrimSpace(line)
		if line != "" && strings.Trim(line, "=-") != "" && !refDef.MatchString(line) {
			return h.Text + "|" + linkDest.ReplaceAllString(line, "]()")
		}
	}
	return h.Text + "|"
}

// headingIndex is the index of the heading frag names, as fetch resolves
// it; -1 when none.
func headingIndex(hs []mdoutline.Heading, frag string) int {
	h, ok := mdoutline.Find(hs, frag)
	if !ok {
		return -1
	}
	return slices.IndexFunc(hs, func(x mdoutline.Heading) bool { return x.Start == h.Start })
}

// find is the signature of the heading frag names.
func find(body string, hs []mdoutline.Heading, frag string) (string, bool) {
	if i := headingIndex(hs, frag); i >= 0 {
		return signature(body, hs, i), true
	}
	return "", false
}

// splitViolations checks a split proposal against the document it splits.
func splitViolations(in *Input, p *Proposal) []string {
	var out []string
	orig := mdoutline.Headings(in.Body)
	written := map[string]string{}
	heads := map[string][]mdoutline.Heading{}
	for _, w := range p.Writes {
		written[w.Path], heads[w.Path] = w.Body, mdoutline.Headings(w.Body)
	}
	// Every heading lands somewhere, level and text kept.
	landed := map[string]int{}
	for _, hs := range heads {
		for _, h := range hs {
			landed[fmt.Sprint(h.Level, h.Text)]++
		}
	}
	for _, h := range orig {
		if k := fmt.Sprint(h.Level, h.Text); landed[k] == 0 {
			out = append(out, fmt.Sprintf("heading %q lost", h.Text))
		} else {
			landed[k]--
		}
	}
	// resolve finds the heading a fragment names in a written file; one the
	// split added (the topics list, a topic's H1) is no original heading.
	resolve := func(path, frag string) (string, bool) {
		hs := heads[path]
		i := headingIndex(hs, frag)
		if i < 0 || !slices.ContainsFunc(orig, func(h mdoutline.Heading) bool { return h.Level == hs[i].Level && h.Text == hs[i].Text }) {
			return "", false
		}
		return signature(written[path], hs, i), true
	}
	sigs := func(path, body string, find func(target, frag string) (string, bool), files map[string]bool) []string {
		var got []string
		for _, dest := range followed(body) {
			to, frag, ok := strings.Cut(dest, "#")
			if to == "" || strings.HasPrefix(to, "?") {
				to = path
			} else if to = links.ResolvePath(path, to); to == "" {
				got = append(got, "url "+dest)
				continue
			}
			if s, found := find(to, frag); ok && files[to] && found {
				got = append(got, s)
			} else {
				got = append(got, "path "+to+"#"+frag)
			}
		}
		return got
	}
	// Every link leads where it did: to the same heading, path or URL.
	before := sigs(in.Path, in.Body, func(_, frag string) (string, bool) {
		return find(in.Body, orig, frag)
	}, map[string]bool{in.Path: true})
	files := map[string]bool{}
	for path := range written {
		files[path] = true
	}
	after := map[string]int{}
	for path, body := range written {
		for _, s := range sigs(path, body, resolve, files) {
			after[s]++
		}
	}
	for _, s := range before {
		if after[s] == 0 {
			out = append(out, fmt.Sprintf("link to %q no longer leads there", s))
		} else {
			after[s]--
		}
	}
	// Moved repoints inbound links to the heading they named.
	for from, to := range p.Moved {
		o, ok := find(in.Body, orig, from)
		path, anchor, _ := strings.Cut(to, "#")
		if s, found := resolve(path, anchor); !ok || !found || s != o {
			out = append(out, fmt.Sprintf("moved %s -> %s, not the same heading", from, to))
		}
	}
	// A hub over the threshold has nothing left to move.
	if hub := written[in.Path]; len(hub) >= outline.Threshold {
		for _, h := range heads[in.Path] {
			if h.Level == 2 && !strings.HasPrefix(h.Text, "Topic") {
				out = append(out, fmt.Sprintf("hub %d bytes still holds %q", len(hub), h.Text))
			}
		}
	}
	return out
}
