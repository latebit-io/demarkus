// Package hiddentext finds characters a document body must not carry: text a
// reader cannot see but a terminal or a model still acts on. The server
// refuses them on write; read surfaces reveal them in documents stored before.
package hiddentext

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Class names why a span is hidden.
type Class string

const (
	// Control is a C0/C1 control or DEL: terminal escapes, lone carriage
	// returns that overwrite a line.
	Control Class = "control"
	// Bidi is an embedding, override or isolate: reorders what a reader sees
	// against what the bytes say.
	Bidi Class = "bidi"
	// LineSeparator is U+2028/U+2029: a line break line-based checks miss.
	LineSeparator Class = "line-separator"
	// Annotation is an interlinear annotation mark.
	Annotation Class = "annotation"
	// Tag is a tag character outside an emoji tag sequence: invisible ASCII.
	Tag Class = "tag"
	// Invisible is a run of zero-width or variation-selector characters no
	// script or emoji sequence writes: an encoding channel.
	Invisible Class = "invisible"
)

// maxHits bounds Find: a refusal names the first few, never every one.
const maxHits = 10

// Hit is one hidden span.
type Hit struct {
	Line, Column int // 1-based; Column counts runes
	First        rune
	Count        int // runes in the span
	Class        Class
}

func (h Hit) String() string {
	if h.Count == 1 {
		return fmt.Sprintf("U+%04X (%s) at line %d, column %d", h.First, h.Class, h.Line, h.Column)
	}
	return fmt.Sprintf("U+%04X (%s, %d characters) at line %d, column %d", h.First, h.Class, h.Count, h.Line, h.Column)
}

// Summary words up to maxHits hits for a refusal.
func Summary(hits []Hit) string {
	parts := make([]string, len(hits))
	for i, h := range hits {
		parts[i] = h.String()
	}
	return strings.Join(parts, "; ")
}

// Has reports whether s carries any hidden span.
func Has(s string) bool {
	found := false
	scan(s, func(int, int, Class) bool { found = true; return false })
	return found
}

// Find returns the first maxHits hidden spans in s, in order.
func Find(s string) []Hit {
	var hits []Hit
	line, lineStart, cursor := 1, 0, 0
	scan(s, func(start, end int, class Class) bool {
		for ; cursor < start; cursor++ {
			if s[cursor] == '\n' {
				line, lineStart = line+1, cursor+1
			}
		}
		first, _ := utf8.DecodeRuneInString(s[start:])
		hits = append(hits, Hit{
			Line: line, Column: utf8.RuneCountInString(s[lineStart:start]) + 1,
			First: first, Count: utf8.RuneCountInString(s[start:end]), Class: class,
		})
		return len(hits) < maxHits
	})
	return hits
}

// Reveal replaces every rune of every hidden span with visible <U+XXXX>
// notation; text without one is returned unchanged.
func Reveal(s string) string {
	if !Has(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	last := 0
	scan(s, func(start, end int, _ Class) bool {
		b.WriteString(s[last:start])
		for _, r := range s[start:end] {
			fmt.Fprintf(&b, "<U+%04X>", r)
		}
		last = end
		return true
	})
	b.WriteString(s[last:])
	return b.String()
}

// scan yields each hidden span as byte offsets [start, end); yield returns
// false to stop.
func scan(s string, yield func(start, end int, class Class) bool) {
	for i := 0; i < len(s); {
		c := s[i]
		// ASCII fast path: printable, tab, newline, CRLF.
		if c < utf8.RuneSelf {
			switch {
			case c >= 0x20 && c != 0x7F, c == '\t', c == '\n':
			case c == '\r' && i+1 < len(s) && s[i+1] == '\n':
			default:
				if !yield(i, i+1, Control) {
					return
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		end, class := i+size, Class("")
		switch {
		case r >= 0x80 && r <= 0x9F:
			class = Control
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			class = Bidi
		case r >= 0x206A && r <= 0x206F: // deprecated format controls, unused by text
			class = Invisible
		case r == 0x2028, r == 0x2029:
			class = LineSeparator
		case r >= 0xFFF9 && r <= 0xFFFB:
			class = Annotation
		case r == 0x1F3F4:
			end += emojiTagSequence(s[end:])
		case isTag(r):
			end, class = runEnd(s, end, isTag), Tag
		case isInvisible(r):
			if end = runEnd(s, end, isInvisible); suspicious(s[i:end]) {
				class = Invisible
			}
		}
		if class != "" && !yield(i, end, class) {
			return
		}
		i = end
	}
}

func isTag(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

func isVariationSelector(r rune) bool {
	return (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0xE0100 && r <= 0xE01EF)
}

// isInvisible is a zero-width or filler character a single legitimate use
// may place (ZWJ in an emoji, ZWNJ in Persian, a BOM), never a long run.
func isInvisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200D, r >= 0x2060 && r <= 0x2064,
		r == 0xFEFF, r == 0x180E, r == 0x115F, r == 0x1160, r == 0x3164, r == 0xFFA0:
		return true
	}
	return isVariationSelector(r)
}

// suspicious reports an invisible run no text writes: three or more, or two
// other than an emoji's variation selector followed by its joiner.
func suspicious(run string) bool {
	n := utf8.RuneCountInString(run)
	if n < 2 {
		return false
	}
	if n > 2 {
		return true
	}
	first, size := utf8.DecodeRuneInString(run)
	second, _ := utf8.DecodeRuneInString(run[size:])
	return !isVariationSelector(first) || second != 0x200D
}

// runEnd advances from i over consecutive runes matching in.
func runEnd(s string, i int, in func(rune) bool) int {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !in(r) {
			break
		}
		i += size
	}
	return i
}

// emojiTagSequence is the byte length of the tag spec plus cancel tag that
// follows a black flag (subdivision flags), or 0 when s does not open one.
func emojiTagSequence(s string) int {
	i, n := 0, 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case (r >= 0xE0030 && r <= 0xE0039) || (r >= 0xE0061 && r <= 0xE007A):
			i, n = i+size, n+1
		case r == 0xE007F && n > 0:
			return i + size
		default:
			return 0
		}
	}
	return 0
}
