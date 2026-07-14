package mdoutline

import (
	"sort"

	"github.com/latebit-io/demarkus/client/links"
)

// AnchoredLink is a body link with its label text and the anchor of the
// enclosing section, for edge provenance.
type AnchoredLink struct {
	Dest   string // destination, fragment stripped (same filtering as links.Extract)
	Label  string // rendered link text, "" for [](x.md)
	Anchor string // enclosing section anchor (no '#'), "" above the first heading
}

// AnchoredLinks returns every link in body with its label and the anchor of
// the innermost heading section containing it. The link set and order match
// links.Extract. Empty-label links attribute by their enclosing block's
// start, which sits in the same section since headings cannot split a block.
func AnchoredLinks(body string) []AnchoredLink {
	hs := Headings(body)
	infos := links.ExtractWithPositions(body)
	out := make([]AnchoredLink, len(infos))
	for i, info := range infos {
		pos := info.OpenBracket
		if pos < 0 {
			pos = info.BlockStart
		}
		anchor := ""
		if pos >= 0 {
			// Headings are in Start order; the last one at or before the
			// link is the innermost enclosing section.
			idx := sort.Search(len(hs), func(j int) bool { return hs[j].Start > pos })
			if idx > 0 {
				anchor = hs[idx-1].Anchor
			}
		}
		out[i] = AnchoredLink{Dest: info.Dest, Label: info.Text, Anchor: anchor}
	}
	return out
}
