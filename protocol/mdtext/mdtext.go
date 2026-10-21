// Package mdtext reduces markdown to the words a catalog indexes and a
// signature hashes: one section split, one markup stripper and one
// tokenizer for every tier, so a lookup and a near-duplicate check agree
// on what a document says.
package mdtext

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol/mdoutline"
)

// MaxTokenBytes drops hashes, long URLs, and similar one-off strings that
// nobody types as a query; their word runs are still emitted.
const MaxTokenBytes = 32

// Piece is one section of a body: its heading, the heading trail down to
// it, and its own text, not its subtree.
type Piece struct {
	Anchor  string
	Heading string
	Trail   []string // ancestor headings then this one; nil for the preamble
	Own     string   // raw markdown of the section's own text
}

// Split cuts src by the shared mdoutline rule. Text before the first
// heading is one bare piece (empty anchor) when it is not blank; a body
// with no headings is that one piece.
func Split(src string) []Piece {
	hs := mdoutline.Headings(src)
	var pieces []Piece
	preambleEnd := len(src)
	if len(hs) > 0 {
		preambleEnd = hs[0].Start
	}
	if preamble := src[:preambleEnd]; strings.TrimSpace(preamble) != "" {
		pieces = append(pieces, Piece{Own: preamble})
	}
	var stack []mdoutline.Heading // ancestors of the current heading
	for i, h := range hs {
		for len(stack) > 0 && stack[len(stack)-1].Level >= h.Level {
			stack = stack[:len(stack)-1]
		}
		trail := make([]string, 0, len(stack)+1)
		for _, a := range stack {
			trail = append(trail, a.Text)
		}
		trail = append(trail, h.Text)
		stack = append(stack, h)

		own := src[headingLineEnd(src, h.Start):h.End]
		if i+1 < len(hs) {
			own = src[headingLineEnd(src, h.Start):hs[i+1].Start]
		}
		pieces = append(pieces, Piece{Anchor: h.Anchor, Heading: h.Text, Trail: trail, Own: own})
	}
	return pieces
}

// headingLineEnd returns the offset just past the heading's first line.
func headingLineEnd(src string, start int) int {
	if nl := strings.IndexByte(src[start:], '\n'); nl >= 0 {
		return start + nl + 1
	}
	return len(src)
}

// Counts tokenizes stripped text: occurrences per token and the total.
func Counts(text string) (counts map[string]int, total int) {
	counts = make(map[string]int)
	for field := range strings.FieldsSeq(text) {
		Tokens(field, func(tok string) {
			counts[tok]++
			total++
		})
	}
	return counts, total
}

// TokenSet tokenizes fields (headings, tags, a title) into a set.
func TokenSet(fields []string) map[string]struct{} {
	seen := make(map[string]struct{})
	for _, f := range fields {
		for field := range strings.FieldsSeq(f) {
			Tokens(field, func(tok string) { seen[tok] = struct{}{} })
		}
	}
	return seen
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func notWordRune(r rune) bool { return !isWordRune(r) }

// Tokens emits a field as the trimmed whole word, then each run of word
// characters in it, so path.Match is matched by path, match, and path.match.
// Tokens are lowercase, at least two runes, and at most MaxTokenBytes.
func Tokens(field string, emit func(string)) {
	whole := strings.ToLower(strings.TrimFunc(field, notWordRune))
	if whole == "" {
		return
	}
	if utf8.RuneCountInString(whole) >= 2 && len(whole) <= MaxTokenBytes {
		emit(whole)
	}
	if strings.IndexFunc(whole, notWordRune) < 0 {
		return
	}
	for part := range strings.FieldsFuncSeq(whole, notWordRune) {
		if utf8.RuneCountInString(part) >= 2 && len(part) <= MaxTokenBytes {
			emit(part)
		}
	}
}

// QueryTerms splits a body-mode query into distinct whole-word terms; a
// field over MaxTokenBytes is queried as its word runs, as it was indexed.
func QueryTerms(query string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(t string) {
		if utf8.RuneCountInString(t) >= 2 && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for field := range strings.FieldsSeq(query) {
		t := strings.ToLower(strings.TrimFunc(field, notWordRune))
		if len(t) > MaxTokenBytes {
			Tokens(t, add)
			continue
		}
		add(t)
	}
	return out
}

var (
	imageRe    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	linkRe     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	htmlTagRe  = regexp.MustCompile(`</?[A-Za-z][^>]*>`)
	listRe     = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(?:\[[ xX]\]\s+)?`)
	quoteRe    = regexp.MustCompile(`^(?:>\s?)+`)
	tableSepRe = regexp.MustCompile(`^\|?[\s|:-]+\|?$`)
	inlineMark = strings.NewReplacer("`", "", "*", "", "~~", "", "|", " ")
)

// Strip reduces markdown to prose lines: fences, list and quote markers,
// link targets, tags, emphasis, and table pipes are dropped so tokens and
// snippets carry words only.
func Strip(raw string) string {
	var lines []string
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") || isSetextUnderline(line) {
			continue
		}
		if strings.HasPrefix(line, "|") && tableSepRe.MatchString(line) {
			continue
		}
		// Each pass is guarded by a byte probe: most prose lines carry no
		// markup, and an unguarded regexp pass allocates even on a miss.
		if line[0] == '>' {
			if line = quoteRe.ReplaceAllString(line, ""); line == "" {
				continue
			}
		}
		if strings.IndexByte("-*+0123456789", line[0]) >= 0 {
			line = listRe.ReplaceAllString(line, "")
		}
		if strings.Contains(line, "](") {
			line = imageRe.ReplaceAllString(line, "$1")
			line = linkRe.ReplaceAllString(line, "$1")
		}
		if strings.IndexByte(line, '<') >= 0 {
			line = htmlTagRe.ReplaceAllString(line, "")
		}
		if strings.ContainsAny(line, "`*~|") {
			line = inlineMark.Replace(line)
		}
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// isSetextUnderline reports a line of only = or - characters, which mdoutline
// counts as part of the heading above it.
func isSetextUnderline(line string) bool {
	return line != "" && (strings.Trim(line, "=") == "" || strings.Trim(line, "-") == "")
}
