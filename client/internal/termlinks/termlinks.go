// Package termlinks finds a document's links in rendered terminal output and
// maps them to screen cells. Positions are kept beside the text, never
// written into it, so no document content can pass for a link boundary.
package termlinks

import (
	"sort"
	"strings"

	runewidth "github.com/mattn/go-runewidth"
)

// Link is one link of the source document as it renders: hidden text in the
// label or destination already revealed.
type Link struct {
	Text string
	Dest string
}

// Span is the extent of link Index in rendered output, as rune offsets
// [Start, End). It covers the label and the URL the renderer prints after it.
type Span struct {
	Index      int
	Start, End int
}

// Region is the cells one line shows of link Index: columns [StartCol, EndCol).
// A link wrapped over several lines has one region per line.
type Region struct {
	Index            int
	Line             int
	StartCol, EndCol int
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

// Locate matches links to the OSC 8 hyperlinks in rendered, in document
// order. A link with no label, or one the renderer gave no hyperlink (a bare
// #anchor), has no span.
func Locate(rendered string, links []Link) []Span {
	if len(links) == 0 {
		return nil
	}
	runes := []rune(rendered)
	found := hyperlinks(runes)
	used := make([]bool, len(found))
	var spans []Span
	for i, link := range links {
		if link.Text == "" {
			continue
		}
		first, last, ok := match(runes, found, used, link)
		if !ok {
			continue
		}
		// The renderer prints the URL after the label as a hyperlink of its own.
		if next := last + 1; next < len(found) && !used[next] && found[next].url == found[last].url && blank(runes[found[last].end:found[next].start]) {
			last = next
		}
		for j := first; j <= last; j++ {
			used[j] = true
		}
		spans = append(spans, Span{Index: i, Start: found[first].start, End: found[last].end})
	}
	sort.Slice(spans, func(a, b int) bool { return spans[a].Start < spans[b].Start })
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

// sameDest compares a rendered hyperlink URL with a source destination; the
// renderer may resolve a relative one and keeps the fragment.
func sameDest(rendered, dest string) bool {
	rendered, _, _ = strings.Cut(rendered, "#")
	return rendered == dest || strings.HasSuffix(rendered, dest) || strings.HasSuffix(dest, rendered)
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

// segment is the part of one span a single line shows, trimmed of the
// spacing a wrap leaves at either end.
type segment struct {
	region     Region
	start, end int // rune offsets [start, end)
}

// Paint highlights the spans lit reports, in reverse video, and returns the
// cells every span occupies. Mouse hit-testing is in cells, not runes: CJK
// and emoji take two columns, combining marks none.
func Paint(rendered string, spans []Span, lit func(index int) bool) (string, []Region) {
	runes := []rune(rendered)
	segments := layout(runes, spans)
	regions := make([]Region, 0, len(segments))
	var marks []mark
	for _, s := range segments {
		regions = append(regions, s.region)
		if lit(s.region.Index) {
			marks = append(marks, mark{s.start, reverseOn}, mark{s.end, reverseOff})
		}
	}
	if len(marks) == 0 {
		return rendered, regions
	}
	return insert(runes, marks), regions
}

// layout walks the text once, tracking line and column, and cuts each span
// into its per-line segments.
func layout(runes []rune, spans []Span) []segment {
	var out []segment
	var cur *segment
	line, col, next := 0, 0, 0
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	scan(runes, func(i int, r rune, visible bool) {
		for next < len(spans) && i >= spans[next].End {
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
		if next < len(spans) && i >= spans[next].Start && r != ' ' {
			if cur == nil {
				cur = &segment{region: Region{Index: spans[next].Index, Line: line, StartCol: col}, start: i}
			}
			cur.end, cur.region.EndCol = i+1, col+width
		}
		col += width
	})
	flush()
	return out
}

// mark is text to insert before the rune at an offset.
type mark struct {
	at   int
	text string
}

// insert writes marks into runes. A style reset inside a lit segment would
// end the highlight, so reverse video is asserted again after each one.
func insert(runes []rune, marks []mark) string {
	var b strings.Builder
	b.Grow(len(runes) + len(marks)*len(reverseOff))
	lit, m := false, 0
	i := 0
	for i <= len(runes) {
		for ; m < len(marks) && marks[m].at == i; m++ {
			b.WriteString(marks[m].text)
			lit = marks[m].text == reverseOn
		}
		if i == len(runes) {
			break
		}
		if after, ok := csi(runes, i); ok && lit && runes[after-1] == 'm' {
			b.WriteString(string(runes[i:after]) + reverseOn)
			i = after
			continue
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}
