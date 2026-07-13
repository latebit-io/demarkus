// Package catalog maintains an in-memory, importance-ranked index of document
// metadata for the LOOKUP verb. It maps each document path to its declared
// tags, importance, title, and modification time, and answers subject lookups
// by matching query terms against tags and titles.
//
// The catalog is derived state: the content store is the source of truth. The
// server rebuilds it by walking current document versions at startup and keeps
// it current by updating it after each successful write. It holds no document
// bodies and performs no body reads at query time.
//
// Read authorization is not a catalog concern. Lookup returns every match; the
// caller filters results by the requester's capabilities before presenting
// them, so a privileged document never leaks through a public lookup.
package catalog

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is a single document's catalog record.
type Entry struct {
	Path       string
	Tags       []string
	Importance float64
	Title      string
	Modified   time.Time
	Metadata   map[string]string // declared publisher metadata, for filter predicates
}

// Result is a ranked LOOKUP match.
type Result struct {
	Entry
	Score int // number of distinct query terms matched in tags or title
}

// Catalog is a concurrency-safe map of document path to catalog entry.
type Catalog struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

// New returns an empty catalog.
func New() *Catalog {
	return &Catalog{entries: make(map[string]*Entry)}
}

// Set adds or replaces the entry for e.Path. The catalog takes ownership of e;
// the caller must not mutate it afterward.
func (c *Catalog) Set(e *Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[e.Path] = e
}

// Put derives an entry from a written document and records it. It is the
// in-memory implementation of the handler's LookupCatalog seam; body is the
// document content with store frontmatter already stripped.
func (c *Catalog) Put(docPath string, meta map[string]string, body []byte, modified time.Time) {
	c.Set(FromDocument(docPath, meta, body, modified))
}

// Remove deletes the entry for the given path, if present.
func (c *Catalog) Remove(docPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, docPath)
}

// Len returns the number of cataloged documents.
func (c *Catalog) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Options narrows and bounds a Lookup.
type Options struct {
	// Scope restricts results to documents under this path prefix. Empty or
	// "/" matches every document.
	Scope string
	// Filter predicates, all of which a document MUST satisfy.
	Filter []Predicate
	// Max caps the number of results returned. Zero means no cap.
	Max int
}

// Lookup returns documents whose tags or title match at least one query term,
// satisfy every filter predicate, and fall under the scope, ordered by
// descending match count, then importance, then modification time, then path.
// The error is always nil; the signature carries it for the handler's
// LookupCatalog seam, where database-backed implementations can fail.
//
// The query "*" matches every catalogued document under the scope (filters
// still apply): with all match scores equal, the ordering reduces to
// importance — "the most important documents here" without guessing a
// subject. This is the whole-catalog view that universe browsers build on.
func (c *Catalog) Lookup(query string, opts Options) ([]Result, error) {
	matchAll := strings.TrimSpace(query) == "*"
	terms := Tokenize(query)
	if !matchAll && len(terms) == 0 {
		return nil, nil
	}
	scope := normalizeScope(opts.Scope)

	c.mu.RLock()
	var results []Result
	for _, e := range c.entries {
		if !underScope(e.Path, scope) {
			continue
		}
		if !MatchesAll(e, opts.Filter) {
			continue
		}
		if matchAll {
			results = append(results, Result{Entry: *e})
			continue
		}
		if score := matchScore(e, terms); score > 0 {
			results = append(results, Result{Entry: *e, Score: score})
		}
	}
	c.mu.RUnlock()

	sortResults(results)
	if opts.Max > 0 && len(results) > opts.Max {
		results = results[:opts.Max]
	}
	return results, nil
}

// Tokenize lowercases s and splits it into distinct whitespace-separated terms,
// preserving first-seen order. Exported so alternative LookupCatalog backends
// tokenize queries identically to the in-memory catalog.
func Tokenize(s string) []string {
	seen := make(map[string]bool)
	var out []string
	for f := range strings.FieldsSeq(strings.ToLower(s)) {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// matchScore counts how many distinct query terms appear in the entry's tags
// (exact, case-insensitive) or title (substring, case-insensitive).
func matchScore(e *Entry, terms []string) int {
	lowerTitle := strings.ToLower(e.Title)
	lowerTags := make(map[string]bool, len(e.Tags))
	for _, t := range e.Tags {
		lowerTags[strings.ToLower(t)] = true
	}
	score := 0
	for _, term := range terms {
		if lowerTags[term] || (lowerTitle != "" && strings.Contains(lowerTitle, term)) {
			score++
		}
	}
	return score
}

// sortResults orders results by descending score, then importance, then
// modification time, then ascending path for a deterministic tiebreak.
func sortResults(rs []Result) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Importance != b.Importance {
			return a.Importance > b.Importance
		}
		if !a.Modified.Equal(b.Modified) {
			return a.Modified.After(b.Modified)
		}
		return a.Path < b.Path
	})
}

// normalizeScope returns a leading-slashed, trailing-slash-trimmed scope, or
// "/" for the whole-server scope.
func normalizeScope(s string) string {
	if s == "" || s == "/" {
		return "/"
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return strings.TrimRight(s, "/")
}

// underScope reports whether docPath falls under scope.
func underScope(docPath, scope string) bool {
	if scope == "/" {
		return true
	}
	return docPath == scope || strings.HasPrefix(docPath, scope+"/")
}
