package catalog

import (
	"maps"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/protocol/mdtext"
	"github.com/latebit-io/demarkus/protocol/neardup"
)

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
	sig     uint64   // near-duplicate signature of the own text; 0 under neardup.MinTokens
}

// DocSections is the section index of one body. It depends on the body
// alone, so a store may carry it across snapshots keyed by body hash.
type DocSections struct {
	vocab    termSet // every token in the sections' text and trails
	sections []section
	sig      uint64 // near-duplicate signature of the whole own text; 0 under neardup.MinTokens
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

// At and Set make the index a neardup.Sections with Len: sets are rebuilt
// from the vocabulary for the few candidates a scan judges, never stored.
func (d *DocSections) At(i int) neardup.Section {
	return neardup.Section{Anchor: d.sections[i].anchor, Sig: d.sections[i].sig}
}

// Set is the token set of one section's own text, or of every section's for -1.
func (d *DocSections) Set(section int) neardup.Set {
	secs := d.sections
	if section >= 0 {
		secs = d.sections[section : section+1]
	}
	return neardup.SetOf(func(yield func(string) bool) {
		for i := range secs {
			for _, id := range secs[i].tokens {
				if !yield(d.vocab.at(int(id))) {
					return
				}
			}
		}
	})
}

// heading is the heading text of the section at anchor, "" when none.
func (d *DocSections) heading(anchor string) string {
	if anchor == "" {
		return ""
	}
	for i := range d.sections {
		if d.sections[i].anchor == anchor {
			return d.sections[i].heading
		}
	}
	return ""
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
func IndexSections(body []byte) *DocSections { return IndexSectionsString(string(body)) }

// IndexSectionsString is IndexSections for a body held as a string, which it
// indexes without a copy.
func IndexSectionsString(src string) *DocSections {
	var raws []rawSection
	for _, p := range mdtext.Split(src) {
		if s, ok := newSection(p.Anchor, p.Heading, p.Own, p.Trail); ok {
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
	var whole neardup.Signer
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
		var signer neardup.Signer
		for tok, c := range raw.counts {
			if len(tok) >= neardup.MinTokenBytes {
				signer.Add(neardup.Hash(tok), c)
			}
		}
		whole.Merge(&signer)
		s.sig = signer.Sig()
		doc.sections[i] = s
	}
	doc.sig = whole.Sig()
	return doc
}

// newSection strips markup and tokenizes own text. A heading with no text of
// its own is still a section: its trail alone can match.
func newSection(anchor, heading, raw string, trail []string) (rawSection, bool) {
	text := mdtext.Strip(raw)
	if text == "" && heading == "" {
		return rawSection{}, false
	}
	counts, total := mdtext.Counts(text)
	// Single-line text can be a substring of the body; the clone keeps the
	// index from pinning the whole body.
	s := section{anchor: anchor, heading: heading, text: strings.Clone(text), length: int32(min(total, 1<<30))}
	return rawSection{sec: s, counts: counts, trail: mdtext.TokenSet(trail)}, true
}

// hasTerm reports whether the sorted id set contains id.
func hasTerm(set []termID, id termID) bool {
	if id == absentTerm {
		return false
	}
	_, ok := slices.BinarySearch(set, id)
	return ok
}
