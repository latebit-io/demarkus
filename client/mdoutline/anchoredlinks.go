package mdoutline

import (
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
// links.Extract.
func AnchoredLinks(body string) []AnchoredLink {
	hs := Headings(body)
	infos := links.ExtractWithPositions(body)
	out := make([]AnchoredLink, len(infos))
	for i, info := range infos {
		anchor := ""
		if info.OpenBracket >= 0 {
			// Headings are in Start order, so the last one at or before the
			// link is the innermost enclosing section.
			for _, h := range hs {
				if h.Start > info.OpenBracket {
					break
				}
				anchor = h.Anchor
			}
		}
		out[i] = AnchoredLink{Dest: info.Dest, Label: info.Text, Anchor: anchor}
	}
	return out
}
