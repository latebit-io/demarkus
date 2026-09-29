package catalog

import (
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol/mdoutline"
)

// maxTokenBytes drops hashes, long URLs, and similar one-off strings that
// nobody types as a query; their word runs are still indexed.
const maxTokenBytes = 32

// termSet is an immutable sorted set of distinct tokens packed into one
// string. It never pins the text its tokens were cut from.
type termSet struct {
	packed string
	ends   []uint32 // token i is packed[ends[i-1]:ends[i]]
}

// newTermSet packs the keys of tokens in sorted order.
func newTermSet[V any](tokens map[string]V) termSet {
	keys := slices.Sorted(maps.Keys(tokens))
	size := 0
	for _, tok := range keys {
		size += len(tok)
	}
	var packed strings.Builder
	packed.Grow(size)
	ends := make([]uint32, len(keys))
	for i, tok := range keys {
		packed.WriteString(tok)
		ends[i] = uint32(packed.Len()) //nolint:gosec // bodies are capped at protocol.MaxBodyLength
	}
	return termSet{packed: packed.String(), ends: ends}
}

func (s termSet) at(i int) string {
	start := uint32(0)
	if i > 0 {
		start = s.ends[i-1]
	}
	return s.packed[start:s.ends[i]]
}

// id returns tok's position in the set, or absentTerm.
func (s termSet) id(tok string) termID {
	i, found := sort.Find(len(s.ends), func(i int) int { return strings.Compare(tok, s.at(i)) })
	if !found {
		return absentTerm
	}
	return termID(i) //nolint:gosec // bodies are capped at protocol.MaxBodyLength
}

func (s termSet) has(tok string) bool { return s.id(tok) != absentTerm }

// termID is a token's position in its document's vocabulary. Ids are
// document-scoped: a process-wide vocabulary would keep every token ever
// indexed, and churned bodies (timestamps, hashes) grow it without bound.
type termID = uint32

// absentTerm is the id of a token the document lacks; no set holds it.
const absentTerm termID = math.MaxUint32

// section is one indexed section: its own text, not its subtree.
type section struct {
	anchor  string
	heading string
	text    string   // markup-stripped own text, one line per source line
	tokens  []termID // distinct own-text tokens, sorted
	tf      []uint8  // occurrences (capped), parallel to tokens
	length  int32    // own-text token count
	trail   []termID // distinct heading-trail tokens, sorted
}

// DocSections is the section index of one body. It depends on the body
// alone, so a store may carry it across snapshots keyed by body hash.
type DocSections struct {
	vocab    termSet // every token in the sections' text and trails
	sections []section
}

// Len returns the number of indexed sections.
func (d *DocSections) Len() int {
	if d == nil {
		return 0
	}
	return len(d.sections)
}

// ids resolves query terms against the document's vocabulary.
func (d *DocSections) ids(terms []string) []termID {
	ids := make([]termID, len(terms))
	for i, t := range terms {
		ids[i] = d.vocab.id(t)
	}
	return ids
}

// rawSection is a section with its tokens still as strings.
type rawSection struct {
	sec    section
	counts map[string]int
	trail  map[string]struct{}
}

// IndexSections splits body by the shared mdoutline rule and tokenizes each
// section's own text and heading trail. Text before the first heading, or a
// body with no headings, is one bare section (empty anchor).
func IndexSections(body []byte) *DocSections {
	src := string(body)
	hs := mdoutline.Headings(src)
	var raws []rawSection
	preambleEnd := len(src)
	if len(hs) > 0 {
		preambleEnd = hs[0].Start
	}
	if s, ok := newSection("", "", src[:preambleEnd], nil); ok {
		raws = append(raws, s)
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
		if s, ok := newSection(h.Anchor, h.Text, own, trail); ok {
			raws = append(raws, s)
		}
	}
	return assignIDs(raws)
}

// assignIDs builds the document vocabulary and converts each section's
// tokens to ids. The vocabulary is sorted, so an id is the token's rank and
// a section's tokens sorted as strings are sorted as ids.
func assignIDs(raws []rawSection) *DocSections {
	ids := make(map[string]termID)
	for i := range raws {
		for tok := range raws[i].counts {
			ids[tok] = 0
		}
		for tok := range raws[i].trail {
			ids[tok] = 0
		}
	}
	doc := &DocSections{vocab: newTermSet(ids), sections: make([]section, len(raws))}
	var rank termID
	for i := range doc.vocab.ends {
		ids[doc.vocab.at(i)] = rank
		rank++
	}
	for i := range raws {
		raw := &raws[i]
		s := raw.sec
		s.tokens = make([]termID, 0, len(raw.counts))
		s.tf = make([]uint8, 0, len(raw.counts))
		for _, tok := range slices.Sorted(maps.Keys(raw.counts)) {
			s.tokens = append(s.tokens, ids[tok])
			s.tf = append(s.tf, uint8(min(raw.counts[tok], 255)))
		}
		s.trail = make([]termID, 0, len(raw.trail))
		for _, tok := range slices.Sorted(maps.Keys(raw.trail)) {
			s.trail = append(s.trail, ids[tok])
		}
		doc.sections[i] = s
	}
	return doc
}

// headingLineEnd returns the offset just past the heading's first line.
func headingLineEnd(src string, start int) int {
	if nl := strings.IndexByte(src[start:], '\n'); nl >= 0 {
		return start + nl + 1
	}
	return len(src)
}

// newSection strips markup and tokenizes own text. A heading with no text of
// its own is still a section: its trail alone can match.
func newSection(anchor, heading, raw string, trail []string) (rawSection, bool) {
	text := stripMarkup(raw)
	if text == "" && heading == "" {
		return rawSection{}, false
	}
	counts := make(map[string]int)
	total := 0
	for field := range strings.FieldsSeq(text) {
		fieldTokens(field, func(tok string) {
			counts[tok]++
			total++
		})
	}
	// Single-line text can be a substring of the body; the clone keeps the
	// index from pinning the whole body.
	s := section{anchor: anchor, heading: heading, text: strings.Clone(text), length: int32(min(total, 1<<30))}
	return rawSection{sec: s, counts: counts, trail: fieldTokenSet(trail)}, true
}

// fieldTokenSet tokenizes fields (headings, tags, a title) into a set.
func fieldTokenSet(fields []string) map[string]struct{} {
	seen := make(map[string]struct{})
	for _, f := range fields {
		for field := range strings.FieldsSeq(f) {
			fieldTokens(field, func(tok string) { seen[tok] = struct{}{} })
		}
	}
	return seen
}

// hasTerm reports whether the sorted id set contains id.
func hasTerm(set []termID, id termID) bool {
	if id == absentTerm {
		return false
	}
	_, ok := slices.BinarySearch(set, id)
	return ok
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func notWordRune(r rune) bool { return !isWordRune(r) }

// fieldTokens emits a field as the trimmed whole word, then each run of word
// characters in it, so path.Match is matched by path, match, and path.match.
// Tokens are lowercase, at least two runes, and at most maxTokenBytes.
func fieldTokens(field string, emit func(string)) {
	whole := strings.ToLower(strings.TrimFunc(field, notWordRune))
	if whole == "" {
		return
	}
	if utf8.RuneCountInString(whole) >= 2 && len(whole) <= maxTokenBytes {
		emit(whole)
	}
	if strings.IndexFunc(whole, notWordRune) < 0 {
		return
	}
	for part := range strings.FieldsFuncSeq(whole, notWordRune) {
		if utf8.RuneCountInString(part) >= 2 && len(part) <= maxTokenBytes {
			emit(part)
		}
	}
}

// queryTerms splits a body-mode query into distinct whole-word terms; a
// field over maxTokenBytes is queried as its word runs, as it was indexed.
func queryTerms(query string) []string {
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
		if len(t) > maxTokenBytes {
			fieldTokens(t, add)
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

// stripMarkup reduces markdown to prose lines: fences, list and quote
// markers, link targets, tags, emphasis, and table pipes are dropped so
// tokens and snippets carry words only.
func stripMarkup(raw string) string {
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
