package catalog

import (
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// BM25 parameters and the weights the reference ranking adds when a term is
// found in the heading trail or in the document's tags and title. Curated
// tags and titles outrank prose hits, as in catalog mode. Order is
// implementation-defined by the spec; only recall and the importance prior
// are contractual.
const (
	bm25K1      = 1.2
	bm25B       = 0.75
	trailWeight = 1.5
	docWeight   = 2.0
)

type bodyCandidate struct {
	entry   *Entry
	sec     *section
	catalog bool // tags or title carry every term: ranks before section rows
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

// lookupBody scans every indexed section under the scope and filter, keeps
// those in which every term appears (own text, heading trail, tags, title),
// and orders them by BM25 scaled by the importance prior. Callers hold the
// read lock.
func (c *Catalog) lookupBody(words []string, scope string, opts Options) []Result {
	if len(words) == 0 {
		return nil
	}
	terms := make([]term, len(words))
	for i, w := range words {
		id, known := lookupTerm(w)
		if !known {
			return nil // never indexed anywhere, so no section holds it
		}
		terms[i] = id
	}
	scan := c.scanBody(terms, scope, opts.Filter)
	if len(scan.found) == 0 {
		return nil
	}
	rankBody(scan, terms)
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

func (c *Catalog) scanBody(terms []term, scope string, filter []Predicate) *bodyScan {
	scan := &bodyScan{df: make([]int, len(terms))}
	totalLength := 0
	docHas := make([]bool, len(terms))
	for path, e := range c.entries {
		if !underScope(e.Path, scope) || !MatchesAll(e, filter) {
			continue
		}
		doc := c.sections[path]
		if doc == nil {
			continue
		}
		docMatches := true
		for i, t := range terms {
			docHas[i] = hasTerm(e.terms, t)
			docMatches = docMatches && docHas[i]
		}
		if docMatches {
			// The catalog answer: tags or title carry every term, so the
			// document is the row, as in catalog mode.
			scan.found = append(scan.found, bodyCandidate{entry: e, sec: &bareSection, catalog: true})
		}
		for i := range doc.sections {
			sec := &doc.sections[i]
			scan.sections++
			totalLength += int(sec.length)
			matched, hits := true, 0
			for j, t := range terms {
				_, inText := termIndex(sec.tokens, t)
				inTrail := hasTerm(sec.trail, t)
				if inText || inTrail {
					scan.df[j]++
					hits++
				}
				if !inText && !inTrail && !docHas[j] {
					matched = false
				}
			}
			if !docMatches && matched && hits > 0 {
				scan.found = append(scan.found, bodyCandidate{entry: e, sec: sec})
			}
		}
	}
	if scan.sections > 0 {
		scan.avgLength = float64(totalLength) / float64(scan.sections)
	}
	return scan
}

// rankBody scores the candidates, normalizes by the best, applies the
// importance prior, and sorts: catalog rows first in importance order, as
// catalog mode would return them, then sections by score, with the spec's
// path-then-anchor tiebreak.
func rankBody(scan *bodyScan, terms []term) {
	found := scan.found
	maxScore := 0.0
	for i := range found {
		found[i].score = bm25(&found[i], terms, scan.df, scan.sections, scan.avgLength)
		maxScore = max(maxScore, found[i].score)
	}
	for i := range found {
		norm := 1.0
		if maxScore > 0 {
			norm = found[i].score / maxScore
		}
		found[i].score = norm * (0.7 + 0.3*found[i].entry.Importance)
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.catalog != b.catalog {
			return a.catalog
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if a.entry.Path != b.entry.Path {
			return a.entry.Path < b.entry.Path
		}
		return a.sec.anchor < b.sec.anchor
	})
}

// bm25 scores one candidate: the text part per term plus the trail and
// document weights when the term was found there.
func bm25(cand *bodyCandidate, terms []term, df []int, sections int, avgLength float64) float64 {
	score := 0.0
	lengthNorm := 1 - bm25B + bm25B*float64(cand.sec.length)/max(avgLength, 1)
	for j, t := range terms {
		idf := math.Log(1 + (float64(sections)-float64(df[j])+0.5)/(float64(df[j])+0.5))
		if i, ok := termIndex(cand.sec.tokens, t); ok {
			tf := float64(cand.sec.tf[i])
			score += idf * tf * (bm25K1 + 1) / (tf + bm25K1*lengthNorm)
		}
		if hasTerm(cand.sec.trail, t) {
			score += idf * trailWeight
		}
		if hasTerm(cand.entry.terms, t) {
			score += idf * docWeight
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
