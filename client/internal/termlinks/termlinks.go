// Package termlinks finds a document's links in rendered terminal output and
// maps them to screen cells. Positions are kept beside the text, never
// written into it, so no document content can pass for a link boundary.
package termlinks

import (
	"sort"
	"strings"
	"unicode/utf8"

	runewidth "github.com/mattn/go-runewidth"
)

// Link is one link of the source document as it renders: hidden text in the
// label or destination already revealed.
type Link struct {
	Text string
	Dest string
}

// span is the extent of link index in rendered output, as rune offsets
// [start, end). It covers the label and the URL the renderer prints after it.
type span struct {
	index      int
	start, end int
}

// Region is the cells one line shows of link Index: columns [StartCol, EndCol).
// A link wrapped over several lines has one region per line.
type Region struct {
	Index            int
	Line             int
	StartCol, EndCol int
}

// Segment is the part of one link a single line shows, trimmed of the spacing
// a wrap leaves at either end: its cells, for the mouse, and its bytes
// [Start, End) in the rendered text, for the highlight.
type Segment struct {
	Region
	Start, End int
}

// Reverse video on and off: how a selected or hovered link is shown.
const (
	reverseOn  = "\x1b[7m"
	reverseOff = "\x1b[27m"
)

// hyperlink is one OSC 8 region: runes [start, end) between its open and close.
type hyperlink struct {
	url, text  string
	start, end int
}

// Locate lays out the links of rendered, in document order: one segment per
// line a link shows on. A link with no label, or one the renderer gave no
// hyperlink (a bare #anchor), has none. Done once per render, not per repaint.
func Locate(rendered string, links []Link) []Segment {
	if len(links) == 0 {
		return nil
	}
	runes := []rune(rendered)
	return layout(runes, spansOf(runes, links))
}

// spansOf matches links to the OSC 8 hyperlinks in runes, in rendered order.
func spansOf(runes []rune, links []Link) []span {
	found := hyperlinks(runes)
	used := make([]bool, len(found))
	var spans []span
	for i, link := range links {
		if link.Text == "" {
			continue
		}
		first, last, ok := match(runes, found, used, link)
		if !ok {
			continue
		}
		// The renderer prints the URL after the label as a hyperlink of its
		// own; another label to the same destination is the next link, not that.
		if next := last + 1; next < len(found) && !used[next] && printsURL(&found[next], found[last].url) && blank(runes[found[last].end:found[next].start]) {
			last = next
		}
		for j := first; j <= last; j++ {
			used[j] = true
		}
		spans = append(spans, span{index: i, start: found[first].start, end: found[last].end})
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].start < spans[b].start })
	return spans
}

// match finds the first unused run of hyperlinks to link's destination whose
// visible text is its label. The renderer reopens the hyperlink around each
// styled part and wrapped line, so a run is joined across that spacing.
func match(runes []rune, found []hyperlink, used []bool, link Link) (first, last int, ok bool) {
	want := squash(link.Text)
	for j := range found {
		if used[j] || !sameDest(found[j].url, link.Dest) {
			continue
		}
		var text strings.Builder
		for k := j; k < len(found) && !used[k] && found[k].url == found[j].url; k++ {
			if k > j {
				gap := runes[found[k-1].end:found[k].start]
				if !blank(gap) {
					break
				}
				if spaced(gap) {
					text.WriteByte(' ')
				}
			}
			text.WriteString(found[k].text)
			if got := squash(text.String()); got == want {
				return j, k, true
			} else if len(got) > len(want) {
				break
			}
		}
	}
	return 0, 0, false
}

// printsURL reports a hyperlink to url whose visible text is that URL, as
// the renderer prints one after a label, wrapped or not.
func printsURL(h *hyperlink, url string) bool {
	return h.url == url && strings.Join(strings.Fields(h.text), "") == url
}

// sameDest compares a rendered hyperlink URL with a source destination; the
// renderer may resolve a relative one and keeps the fragment. An empty side
// (a bare #anchor) matches only itself: every URL ends in the empty string.
func sameDest(rendered, dest string) bool {
	rendered, _, _ = strings.Cut(rendered, "#")
	if rendered == dest {
		return true
	}
	return rendered != "" && dest != "" && (strings.HasSuffix(rendered, dest) || strings.HasSuffix(dest, rendered))
}

// squash folds runs of whitespace, so a wrapped label equals its source.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// blank reports runes that show nothing but spacing: escapes and whitespace.
func blank(runes []rune) bool {
	empty := true
	scan(runes, func(_ int, r rune, visible bool) {
		if visible && !isSpace(r) {
			empty = false
		}
	})
	return empty
}

// spaced reports runes that show at least one space or line break.
func spaced(runes []rune) bool {
	found := false
	scan(runes, func(_ int, r rune, visible bool) {
		found = found || (visible && isSpace(r))
	})
	return found
}

func isSpace(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }

// hyperlinks lists the OSC 8 regions of runes with their visible text.
func hyperlinks(runes []rune) []hyperlink {
	var out []hyperlink
	var open *hyperlink
	var text strings.Builder
	i := 0
	for i < len(runes) {
		if content, next, ok := osc(runes, i); ok {
			if url, isLink := hyperlinkURL(content); isLink {
				if open != nil {
					open.end, open.text = i, text.String()
					out = append(out, *open)
					open = nil
				}
				if url != "" {
					open = &hyperlink{url: url, start: next}
					text.Reset()
				}
			}
			i = next
			continue
		}
		if next, ok := csi(runes, i); ok {
			i = next
			continue
		}
		if open != nil {
			text.WriteRune(runes[i])
		}
		i++
	}
	return out
}

// hyperlinkURL reads an OSC 8 payload "8;params;url"; an empty URL closes.
func hyperlinkURL(content string) (url string, ok bool) {
	rest, found := strings.CutPrefix(content, "8;")
	if !found {
		return "", false
	}
	_, url, ok = strings.Cut(rest, ";")
	return url, ok
}

// osc reads an OSC sequence at i: its payload and the index after its
// terminator (BEL or ESC \). An unterminated one runs to the end.
func osc(runes []rune, i int) (content string, next int, ok bool) {
	if i+1 >= len(runes) || runes[i] != '\x1b' || runes[i+1] != ']' {
		return "", i, false
	}
	for end := i + 2; end < len(runes); end++ {
		switch {
		case runes[end] == '\x07':
			return string(runes[i+2 : end]), end + 1, true
		case runes[end] == '\x1b' && end+1 < len(runes) && runes[end+1] == '\\':
			return string(runes[i+2 : end]), end + 2, true
		}
	}
	return string(runes[i+2:]), len(runes), true
}

// csi reads a CSI sequence at i and returns the index after its final byte.
func csi(runes []rune, i int) (next int, ok bool) {
	if i+1 >= len(runes) || runes[i] != '\x1b' || runes[i+1] != '[' {
		return i, false
	}
	for end := i + 2; end < len(runes); end++ {
		if runes[end] >= 0x40 && runes[end] <= 0x7e {
			return end + 1, true
		}
	}
	return len(runes), true
}

// scan calls visit for every rune; visible is false inside an escape sequence.
func scan(runes []rune, visit func(i int, r rune, visible bool)) {
	i := 0
	for i < len(runes) {
		next, isEscape := i, false
		if _, after, ok := osc(runes, i); ok {
			next, isEscape = after, true
		} else if after, ok := csi(runes, i); ok {
			next, isEscape = after, true
		}
		if !isEscape {
			visit(i, runes[i], true)
			i++
			continue
		}
		for ; i < next; i++ {
			visit(i, runes[i], false)
		}
	}
}

// Paint highlights the segments lit reports, in reverse video. Mouse
// hit-testing reads the segments' cells, not runes: CJK and emoji take two
// columns, combining marks none.
func Paint(rendered string, segments []Segment, lit func(index int) bool) string {
	var b strings.Builder
	at := 0
	for i := range segments {
		seg := &segments[i]
		if !lit(seg.Index) {
			continue
		}
		if b.Len() == 0 {
			b.Grow(len(rendered) + len(segments)*len(reverseOn+reverseOff))
		}
		b.WriteString(rendered[at:seg.Start])
		b.WriteString(reverseOn)
		writeLit(&b, rendered[seg.Start:seg.End])
		b.WriteString(reverseOff)
		at = seg.End
	}
	if b.Len() == 0 {
		return rendered
	}
	b.WriteString(rendered[at:])
	return b.String()
}

// writeLit copies a lit segment. A style reset inside it would end the
// highlight, so reverse video is asserted again after each style sequence.
func writeLit(b *strings.Builder, text string) {
	for {
		i := strings.Index(text, "\x1b[")
		if i < 0 {
			b.WriteString(text)
			return
		}
		end := i + 2
		for end < len(text) && (text[end] < 0x40 || text[end] > 0x7e) {
			end++
		}
		end = min(end+1, len(text))
		b.WriteString(text[:end])
		if text[end-1] == 'm' {
			b.WriteString(reverseOn)
		}
		text = text[end:]
	}
}

// layout walks the text once, tracking line, column and byte offset, and cuts
// each span into its per-line segments.
func layout(runes []rune, spans []span) []Segment {
	var out []Segment
	var cur *Segment
	line, col, next, offset := 0, 0, 0, 0
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	scan(runes, func(i int, r rune, visible bool) {
		start := offset
		offset += utf8.RuneLen(r)
		for next < len(spans) && i >= spans[next].end {
			flush()
			next++
		}
		if !visible {
			return
		}
		if r == '\n' {
			flush()
			line, col = line+1, 0
			return
		}
		width := runewidth.RuneWidth(r)
		if next < len(spans) && i >= spans[next].start && r != ' ' {
			if cur == nil {
				cur = &Segment{Index: spans[next].index, Line: line, StartCol: col, Start: start}
			}
			cur.End, cur.EndCol = offset, col+width
		}
		col += width
	})
	flush()
	return out
}
