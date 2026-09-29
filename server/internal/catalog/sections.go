package catalog

import (
	"iter"
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

// newTermSet packs distinct tokens; it sorts tokens in place. Tokens past
// 4 GiB are dropped, which a 1 MiB body cannot reach.
func newTermSet(tokens []string) termSet {
	if len(tokens) == 0 {
		return termSet{}
	}
	slices.Sort(tokens)
	var packed strings.Builder
	ends := make([]uint32, 0, len(tokens))
	for _, tok := range tokens {
		end := packed.Len() + len(tok)
		if end >= math.MaxUint32 {
			break
		}
		packed.WriteString(tok)
		ends = append(ends, uint32(end)) //nolint:gosec // bounded by the check above
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
	i := sort.Search(len(s.ends), func(i int) bool { return s.at(i) >= tok })
	if i >= len(s.ends) || i >= math.MaxUint32 || s.at(i) != tok {
		return absentTerm
	}
	return termID(i) //nolint:gosec // bounded by the check above
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

// rawSection is a section before its tokens have document ids.
type rawSection struct {
	section
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

// assignIDs builds the document vocabulary and gives each section its
// tokens as sorted ids.
func assignIDs(raws []rawSection) *DocSections {
	all := make(map[string]struct{})
	for i := range raws {
		for tok := range raws[i].counts {
			all[tok] = struct{}{}
		}
		for tok := range raws[i].trail {
			all[tok] = struct{}{}
		}
	}
	doc := &DocSections{vocab: newTermSet(slices.Collect(maps.Keys(all))), sections: make([]section, len(raws))}
	for i := range raws {
		raw := &raws[i]
		s := raw.section
		s.tokens = vocabIDs(doc.vocab, maps.Keys(raw.counts))
		s.tf = make([]uint8, len(s.tokens))
		for j, id := range s.tokens {
			s.tf[j] = uint8(min(raw.counts[doc.vocab.at(int(id))], 255))
		}
		s.trail = vocabIDs(doc.vocab, maps.Keys(raw.trail))
		doc.sections[i] = s
	}
	return doc
}

// vocabIDs returns the sorted ids of tokens, skipping any the vocabulary
// dropped.
func vocabIDs(vocab termSet, tokens iter.Seq[string]) []termID {
	var ids []termID
	for tok := range tokens {
		if id := vocab.id(tok); id != absentTerm {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
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
	s := section{anchor: anchor, heading: heading, text: text, length: int32(min(total, 1<<30))}
	return rawSection{section: s, counts: counts, trail: fieldTokenSet(trail)}, true
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

// fieldTerms packs the tokens of fields (tags and a title).
func fieldTerms(fields []string) termSet {
	return newTermSet(slices.Collect(maps.Keys(fieldTokenSet(fields))))
}

// hasTerm reports whether the sorted id set contains id.
func hasTerm(set []termID, id termID) bool {
	_, ok := termIndex(set, id)
	return ok
}

func termIndex(set []termID, id termID) (int, bool) {
	i := sort.Search(len(set), func(i int) bool { return set[i] >= id })
	return i, i < len(set) && set[i] == id
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
