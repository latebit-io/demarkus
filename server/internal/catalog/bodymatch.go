package catalog

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

// BM25 parameters plus the weights added for a term found in the heading
// trail or in tags and title. Order is implementation-defined by the spec;
// only recall and the importance prior are contractual.
const (
	bm25K1      = 1.2
	bm25B       = 0.75
	trailWeight = 1.5
	docWeight   = 2.0
)

// Reference ranking knobs tuned on the retrieval benchmark (plan:
// token-efficient retrieval, workstream 3): the importance prior's floor,
// the boost for a tags-or-title match, and the multiplier under journal/.
const (
	priorFloor    = 0.3
	catalogBoost  = 2.0
	journalDemote = 0.5
)

type bodyCandidate struct {
	entry   *Entry
	sec     *section
	ids     []termID // the query terms in this document's vocabulary
	catalog bool     // tags or title carry every term: one boosted bare-path row
	score   float64
}

// bareSection is the row for a document matched by tags or title alone.
var bareSection = section{}

// bodyScan is one pass over the sections in scope: the candidates in which
// every term appears, plus the corpus statistics BM25 needs.
type bodyScan struct {
	found     []bodyCandidate
	df        []int // sections holding each term in text or trail
	sections  int
	avgLength float64
}

// lookupBody keeps every section under the scope and filter in which all
// terms appear (text, heading trail, tags, title), ordered by BM25 scaled by
// the importance prior.
func lookupBody(index Index, words []string, scope string, opts Options) []Result {
	if len(words) == 0 {
		return nil
	}
	scan := scanBody(index, words, scope, opts.Filter)
	if len(scan.found) == 0 {
		return nil
	}
	rankBody(scan, words)
	found := scan.found
	if opts.Max > 0 && len(found) > opts.Max {
		found = found[:opts.Max]
	}
	results := make([]Result, len(found))
	for i, f := range found {
		results[i] = Result{
			Entry:   *f.entry,
			Anchor:  f.sec.anchor,
			Heading: f.sec.heading,
			Snippet: snippet(f.sec.text, words),
		}
	}
	return results
}

func scanBody(index Index, terms []string, scope string, filter []Predicate) *bodyScan {
	scan := &bodyScan{df: make([]int, len(terms))}
	totalLength := 0
	docHas := make([]bool, len(terms))
	for e, doc := range index.Entries(scope) {
		if !underScope(e.Path, scope) || !MatchesAll(e, filter) {
			continue
		}
		if doc == nil {
			continue
		}
		ids := doc.ids(terms)
		docMatches := true
		for i, t := range terms {
			docHas[i] = e.terms.has(t)
			docMatches = docMatches && docHas[i]
		}
		if docMatches {
			// The catalog answer: tags or title carry every term, so the
			// document is the row, as in catalog mode.
			scan.found = append(scan.found, bodyCandidate{entry: e, sec: &bareSection, catalog: true, ids: ids})
		}
		for i := range doc.sections {
			sec := &doc.sections[i]
			scan.sections++
			totalLength += int(sec.length)
			matched, hits := true, 0
			for j, id := range ids {
				inText := hasTerm(sec.tokens, id)
				inTrail := hasTerm(sec.trail, id)
				if inText || inTrail {
					scan.df[j]++
					hits++
				}
				if !inText && !inTrail && !docHas[j] {
					matched = false
				}
			}
			if !docMatches && matched && hits > 0 {
				scan.found = append(scan.found, bodyCandidate{entry: e, sec: sec, ids: ids})
			}
		}
	}
	if scan.sections > 0 {
		scan.avgLength = float64(totalLength) / float64(scan.sections)
	}
	return scan
}

// rankBody scores (catalog rows boosted), normalizes by the best, applies
// the prior, and sorts by score, then the spec's path-then-anchor tiebreak.
func rankBody(scan *bodyScan, terms []string) {
	found := scan.found
	idf := idfs(scan, len(terms))
	maxScore := 0.0
	for i := range found {
		found[i].score = bm25(&found[i], terms, idf, scan.avgLength)
		if found[i].catalog {
			found[i].score *= catalogBoost
		}
		maxScore = max(maxScore, found[i].score)
	}
	for i := range found {
		norm := 1.0
		if maxScore > 0 {
			norm = found[i].score / maxScore
		}
		found[i].score = norm * prior(found[i].entry)
	}
	slices.SortStableFunc(found, func(a, b bodyCandidate) int {
		if a.score != b.score {
			return cmp.Compare(b.score, a.score)
		}
		if c := cmp.Compare(a.entry.Path, b.entry.Path); c != 0 {
			return c
		}
		return cmp.Compare(a.sec.anchor, b.sec.anchor)
	})
}

// prior scales a normalized score by declared importance, then by the
// document's path demotion.
func prior(e *Entry) float64 {
	return (priorFloor + (1-priorFloor)*e.Importance) * e.demote
}

// idfs is the BM25 inverse document frequency per term over the scanned
// sections, fixed for the query.
func idfs(scan *bodyScan, n int) []float64 {
	idf := make([]float64, n)
	sections := float64(scan.sections)
	for j := range idf {
		df := float64(scan.df[j])
		idf[j] = math.Log(1 + (sections-df+0.5)/(df+0.5))
	}
	return idf
}

// bm25 scores one candidate: the text part per term plus the trail and
// document weights when the term was found there; idf is per term.
func bm25(cand *bodyCandidate, terms []string, idf []float64, avgLength float64) float64 {
	score := 0.0
	lengthNorm := 1 - bm25B + bm25B*float64(cand.sec.length)/max(avgLength, 1)
	for j, t := range terms {
		if i, ok := slices.BinarySearch(cand.sec.tokens, cand.ids[j]); ok {
			tf := float64(cand.sec.tf[i])
			score += idf[j] * tf * (bm25K1 + 1) / (tf + bm25K1*lengthNorm)
		}
		if hasTerm(cand.sec.trail, cand.ids[j]) {
			score += idf[j] * trailWeight
		}
		if cand.entry.terms.has(t) {
			score += idf[j] * docWeight
		}
	}
	return score
}

// snippet picks the line of text showing the most query terms (the first
// line when none does) and caps it at SnippetBytes on a rune boundary.
func snippet(text string, terms []string) string {
	best, bestHits := "", -1
	for line := range strings.SplitSeq(text, "\n") {
		lower := strings.ToLower(line)
		hits := 0
		for _, t := range terms {
			if strings.Contains(lower, t) {
				hits++
			}
		}
		if hits > bestHits {
			best, bestHits = line, hits
		}
	}
	if len(best) <= SnippetBytes {
		return best
	}
	cut := SnippetBytes - len("...")
	for cut > 0 && !utf8.RuneStart(best[cut]) {
		cut--
	}
	return best[:cut] + "..."
}
