// Package neardup finds the documents a text repeats: a simhash signature per
// text selects candidates in one pass over 8 bytes each, exact token overlap
// judges them. Tokens come from mdtext so every tier agrees on them; the
// scan, the verdict and its wording live here so every tier agrees on those.
package neardup

import (
	"cmp"
	"iter"
	"math/bits"
	"slices"
)

// Thresholds. Constants until a benchmark on real worlds says they differ
// per world; a near-duplicate is a judgment, so no tier blocks on one.
const (
	// MinTokens is the kept-token count under which a text has no
	// signature: short texts collide.
	MinTokens = 40
	// MinTokenBytes drops the short function words that every text shares.
	MinTokenBytes = 3
	// MaxDistance is the Hamming distance that makes a candidate: a 15%
	// rewording of an 80-token paragraph lands at 5.
	MaxDistance = 6
	// NearDuplicate is the Jaccard overlap of the same text reworded; the
	// same paragraph scores 0.76 on token sets.
	NearDuplicate = 0.7
	// Contained is the containment at which the draft is already a part of
	// a longer text.
	Contained = 0.9
	// maxCandidates bounds the exact comparisons one scan runs: the closest
	// signatures are judged, the rest are not candidates.
	maxCandidates = 20
)

// OverrideKey is the publisher metadata key that keeps a near-duplicate:
// the reason is stored with the version, the gate stays silent.
const OverrideKey = "near-duplicate-override"

// CuratedFromKey names the document a split topic came out of. The gate
// excuses a hit on it with a note: the topic lands before the hub drops it.
const CuratedFromKey = "curated-from"

// Verdict is the judgment on one target, ordered by strength.
type Verdict uint8

// Verdicts, weakest first; None is no overlap worth naming.
const (
	None Verdict = iota
	VerdictContained
	VerdictNearDuplicate
)

// Hash is FNV-1a 64 over the token bytes.
func Hash(tok string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(tok); i++ {
		h ^= uint64(tok[i])
		h *= 1099511628211
	}
	return h
}

// Signer accumulates a term-frequency weighted simhash; one text's tokens
// may feed several signers, hashed once.
type Signer struct {
	v    [64]int
	kept int
}

// Add weighs a hashed token count times, capped so one repeated token does
// not own the signature.
func (s *Signer) Add(h uint64, count int) {
	s.kept += count
	w := min(count, 255)
	for bit := range &s.v {
		if h>>bit&1 == 1 {
			s.v[bit] += w
		} else {
			s.v[bit] -= w
		}
	}
}

// Merge adds another signer's weight, so a whole text is the sum of its
// sections without hashing their tokens again.
func (s *Signer) Merge(o *Signer) {
	s.kept += o.kept
	for bit := range &s.v {
		s.v[bit] += o.v[bit]
	}
}

// Sig is the signature, 0 for a text under MinTokens.
func (s *Signer) Sig() uint64 {
	if s.kept < MinTokens {
		return 0
	}
	var sig uint64
	for bit, weight := range &s.v {
		if weight > 0 {
			sig |= 1 << bit
		}
	}
	return sig
}

// Hamming is the number of bits on which two signatures differ.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// Set is the sorted distinct hashes of a text's kept tokens.
type Set []uint64

// SetOf hashes the kept tokens of a sequence.
func SetOf(tokens iter.Seq[string]) Set {
	var s Set
	for tok := range tokens {
		if len(tok) >= MinTokenBytes {
			s = append(s, Hash(tok))
		}
	}
	return s.normalized()
}

// normalized sorts and dedupes in place.
func (s Set) normalized() Set {
	slices.Sort(s)
	return slices.Compact(s)
}

// intersection counts the hashes two sorted sets share.
func intersection(a, b Set) int {
	n, i, j := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			n++
			i++
			j++
		}
	}
	return n
}

// Jaccard is the share of the union both sets hold; 0 for two empty sets.
func (s Set) Jaccard(b Set) float64 {
	union := len(s) + len(b)
	if union == 0 {
		return 0
	}
	n := intersection(s, b)
	return float64(n) / float64(union-n)
}

// Text is one signed text: a whole document or one section of it.
type Text struct {
	Anchor string // "" for a whole document or the preamble
	Sig    uint64 // 0 under MinTokens
	Set    Set
}

// Target is one document a scan compares a draft with: its whole-text
// signature and its sections, read in place, so a scan allocates nothing per
// document and a catalog holds the 8-byte signatures and no sets.
type Target struct {
	Path     string
	Sig      uint64
	Sections Sections
}

// Section is one section's anchor and signature.
type Section struct {
	Anchor string
	Sig    uint64
}

// Sections reads a target's sections and resolves token sets on demand;
// Set(-1) is the whole text's.
type Sections interface {
	Len() int
	At(i int) Section
	Set(section int) Set
}

// Hit is one document the draft repeats. Anchor names the section when the
// draft is one section of a longer document.
type Hit struct {
	Path    string
	Anchor  string
	Verdict Verdict
	Score   float64 // Jaccard for near-duplicate, containment for contained
}

// Location is path, or path#anchor for a section.
func (h Hit) Location() string {
	if h.Anchor == "" {
		return h.Path
	}
	return h.Path + "#" + h.Anchor
}

type candidate struct {
	target   Target
	section  int // -1 for the whole document
	distance int
}

// Scan finds the targets the draft repeats: signatures within MaxDistance
// select candidates, exact overlap on the closest judges them, one hit per
// path, strongest first. A target with one section is judged as a whole only.
func Scan(draft Text, targets iter.Seq[Target]) []Hit {
	if draft.Sig == 0 {
		return nil
	}
	var found []candidate
	for t := range targets {
		if t.Sig != 0 {
			if d := Hamming(draft.Sig, t.Sig); d <= MaxDistance {
				found = append(found, candidate{target: t, section: -1, distance: d})
			}
		}
		// One section is the whole document, judged above.
		if t.Sections == nil || t.Sections.Len() < 2 {
			continue
		}
		for i := range t.Sections.Len() {
			s := t.Sections.At(i)
			if s.Sig == 0 {
				continue
			}
			if d := Hamming(draft.Sig, s.Sig); d <= MaxDistance {
				found = append(found, candidate{target: t, section: i, distance: d})
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	slices.SortStableFunc(found, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.distance, b.distance), cmp.Compare(a.target.Path, b.target.Path), cmp.Compare(a.section, b.section))
	})
	found = found[:min(len(found), maxCandidates)]

	best := make(map[string]Hit)
	for _, c := range found {
		hit, ok := judge(draft.Set, c.target.Sections.Set(c.section), c.section >= 0)
		if !ok {
			continue
		}
		hit.Path = c.target.Path
		if c.section >= 0 {
			hit.Anchor = c.target.Sections.At(c.section).Anchor
		}
		if prev, seen := best[hit.Path]; !seen || stronger(hit, prev) {
			best[hit.Path] = hit
		}
	}
	hits := slices.Collect(func(yield func(Hit) bool) {
		for _, h := range best {
			if !yield(h) {
				return
			}
		}
	})
	slices.SortFunc(hits, func(a, b Hit) int {
		return cmp.Or(cmp.Compare(b.Verdict, a.Verdict), cmp.Compare(b.Score, a.Score), cmp.Compare(a.Path, b.Path))
	})
	return hits
}

// judge is the verdict on one target set: a section repeating the draft means
// the document holds it (contained, by containment); a whole document is a
// near-duplicate by Jaccard, or contains the draft when it is the longer one.
func judge(draft, target Set, section bool) (Hit, bool) {
	if len(draft) == 0 {
		return Hit{}, false
	}
	n := intersection(draft, target)
	jaccard := float64(n) / float64(len(draft)+len(target)-n)
	containment := float64(n) / float64(len(draft))
	switch {
	case section && (jaccard >= NearDuplicate || containment >= Contained):
		return Hit{Verdict: VerdictContained, Score: containment}, true
	case !section && jaccard >= NearDuplicate:
		return Hit{Verdict: VerdictNearDuplicate, Score: jaccard}, true
	case !section && len(target) > len(draft) && containment >= Contained:
		return Hit{Verdict: VerdictContained, Score: containment}, true
	}
	return Hit{}, false
}

// stronger prefers the higher verdict, then the whole document over a
// section, then the higher score.
func stronger(a, b Hit) bool {
	if a.Verdict != b.Verdict {
		return a.Verdict > b.Verdict
	}
	if (a.Anchor == "") != (b.Anchor == "") {
		return a.Anchor == ""
	}
	return a.Score > b.Score
}
