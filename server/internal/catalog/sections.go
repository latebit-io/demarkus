package catalog

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol/mdoutline"
)

// term is a token id in the process-wide vocabulary, so a DocSections built
// for one snapshot is valid in the next and each token string is stored once.
type term = uint32

// maxTokenBytes drops hashes, long URLs, and similar one-off strings that
// nobody types as a query; their word runs are still indexed.
const maxTokenBytes = 32

// vocabulary interns token strings process-wide. It only grows: bounded by
// distinct tokens ever indexed, which the length cap keeps small.
var vocabulary = struct {
	mu    sync.RWMutex
	ids   map[string]term
	words []string
}{ids: make(map[string]term)}

// internTerms resolves tokens to ids, adding unknown ones, under one lock.
func internTerms(tokens []string) []term {
	ids := make([]term, len(tokens))
	vocabulary.mu.Lock()
	defer vocabulary.mu.Unlock()
	for i, tok := range tokens {
		id, ok := vocabulary.ids[tok]
		if !ok {
			id = term(len(vocabulary.words))
			vocabulary.words = append(vocabulary.words, tok)
			vocabulary.ids[tok] = id
		}
		ids[i] = id
	}
	return ids
}

// lookupTerm resolves a query token without adding it; a token the
// vocabulary has never seen cannot be in any section.
func lookupTerm(tok string) (term, bool) {
	vocabulary.mu.RLock()
	defer vocabulary.mu.RUnlock()
	id, ok := vocabulary.ids[tok]
	return id, ok
}

// section is one indexed section: its own text, not its subtree.
type section struct {
	anchor  string
	heading string
	text    string  // markup-stripped own text, one line per source line
	tokens  []term  // distinct own-text tokens, sorted by id
	tf      []uint8 // occurrences (capped), parallel to tokens
	length  int32   // own-text token count
	trail   []term  // distinct heading-trail tokens, sorted by id
}

// DocSections is the section index of one body. It depends on the body
// alone, so a store may carry it across snapshots keyed by body hash.
type DocSections struct {
	sections []section
}

// Len returns the number of indexed sections.
func (d *DocSections) Len() int {
	if d == nil {
		return 0
	}
	return len(d.sections)
}

// IndexSections splits body by the shared mdoutline rule and tokenizes each
// section's own text and heading trail. Text before the first heading, or a
// body with no headings, is one bare section (empty anchor).
func IndexSections(body []byte) *DocSections {
	src := string(body)
	hs := mdoutline.Headings(src)
	doc := &DocSections{}
	preambleEnd := len(src)
	if len(hs) > 0 {
		preambleEnd = hs[0].Start
	}
	if s, ok := newSection("", "", src[:preambleEnd], nil); ok {
		doc.sections = append(doc.sections, s)
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
			doc.sections = append(doc.sections, s)
		}
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
func newSection(anchor, heading, raw string, trail []string) (section, bool) {
	text := stripMarkup(raw)
	if text == "" && heading == "" {
		return section{}, false
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
	s.tokens, s.tf = sortedTerms(counts)
	s.trail = termSet(trail)
	return s, true
}

// sortedTerms interns the tokens and returns them sorted by id with their
// capped counts alongside.
func sortedTerms(counts map[string]int) (ids []term, tf []uint8) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	interned := internTerms(keys)
	order := make([]int, len(keys))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return interned[order[a]] < interned[order[b]] })
	ids = make([]term, len(keys))
	tf = make([]uint8, len(keys))
	for i, k := range order {
		ids[i] = interned[k]
		tf[i] = uint8(min(counts[keys[k]], 255))
	}
	return ids, tf
}

// termSet tokenizes fields (headings, tags, a title) into an id-sorted set.
func termSet(fields []string) []term {
	seen := make(map[string]struct{})
	for _, f := range fields {
		for field := range strings.FieldsSeq(f) {
			fieldTokens(field, func(tok string) { seen[tok] = struct{}{} })
		}
	}
	if len(seen) == 0 {
		return nil
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	ids := internTerms(keys)
	slices.Sort(ids)
	return ids
}

// hasTerm reports whether the id-sorted set contains id.
func hasTerm(set []term, id term) bool {
	_, ok := termIndex(set, id)
	return ok
}

func termIndex(set []term, id term) (int, bool) {
	i := sort.Search(len(set), func(i int) bool { return set[i] >= id })
	return i, i < len(set) && set[i] == id
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func notWordRune(r rune) bool { return !isWordRune(r) }

// fieldTokens emits one whitespace-separated field as the whole word with
// surrounding punctuation trimmed, then each run of word characters inside
// it, so path.Match is matched by path, match, and path.match. Tokens are
// lowercase and at least two runes.
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

// queryTerms splits a body-mode query into distinct whole-word terms.
func queryTerms(query string) []string {
	seen := make(map[string]bool)
	var out []string
	for field := range strings.FieldsSeq(query) {
		t := strings.ToLower(strings.TrimFunc(field, notWordRune))
		if utf8.RuneCountInString(t) < 2 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
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
	spaceRe    = regexp.MustCompile(`\s+`)
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
		line = quoteRe.ReplaceAllString(line, "")
		line = listRe.ReplaceAllString(line, "")
		line = imageRe.ReplaceAllString(line, "$1")
		line = linkRe.ReplaceAllString(line, "$1")
		line = htmlTagRe.ReplaceAllString(line, "")
		line = strings.NewReplacer("`", "", "*", "", "~~", "", "|", " ").Replace(line)
		line = strings.TrimSpace(spaceRe.ReplaceAllString(line, " "))
		if line != "" {
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
