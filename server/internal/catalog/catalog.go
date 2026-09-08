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
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
)

// Entry is a single document's catalog record.
type Entry struct {
	Path       string
	Tags       []string
	Importance float64
	Title      string
	Modified   time.Time
	Metadata   map[string]string // declared publisher metadata, for filter predicates

	terms  []term  // tag and title tokens for body match; set by Set
	demote float64 // body-match prior multiplier from the path; set by Set
}

// Result is a ranked LOOKUP match. Anchor, Heading, and Snippet are set
// only in body mode; the bare-path section of a document leaves Anchor and
// Heading empty.
type Result struct {
	Entry
	Score   int    // catalog mode: distinct query terms matched in tags or title
	Anchor  string // body mode: section anchor without '#'
	Heading string // body mode: heading text of the section
	Snippet string // body mode: one line of section text, at most SnippetBytes
}

// Location is the row's path, with #anchor when the row is a section.
func (r *Result) Location() string {
	if r.Anchor == "" {
		return r.Path
	}
	return r.Path + "#" + r.Anchor
}

// DisplayTitle is the document title, joined to the heading for a section.
func (r *Result) DisplayTitle() string {
	if r.Anchor == "" {
		return r.Title
	}
	return r.Title + " › " + r.Heading
}

// SnippetBytes caps a body-mode snippet, per the spec.
const SnippetBytes = 240

// Mode selects what a Lookup matches against.
type Mode string

// Lookup modes. The empty Mode is catalog mode.
const (
	MatchCatalog Mode = "catalog" // declared tags and title
	MatchBody    Mode = "body"    // the section index
)

// ParseMode maps the wire value of the match key to a Mode; "" is catalog.
func ParseMode(s string) (Mode, error) {
	value, err := protocol.ParseMatch(s)
	if err != nil {
		return "", err
	}
	return Mode(value), nil
}

// Catalog is a concurrency-safe map of document path to catalog entry, with
// the section index for body match beside it.
type Catalog struct {
	mu       sync.RWMutex
	entries  map[string]*Entry
	sections map[string]*DocSections
}

// New returns an empty catalog.
func New() *Catalog {
	return &Catalog{entries: make(map[string]*Entry), sections: make(map[string]*DocSections)}
}

// Set adds or replaces the entry for e.Path, leaving its section index as it
// was. The catalog takes ownership of e; the caller must not mutate it.
func (c *Catalog) Set(e *Entry) {
	e.prepare()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[e.Path] = e
}

// Put derives an entry from a written document and indexes its sections;
// body has store frontmatter already stripped.
func (c *Catalog) Put(docPath string, meta map[string]string, body []byte, modified time.Time) {
	e := FromDocument(docPath, meta, body, modified)
	e.prepare()
	doc := IndexSections(body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[e.Path] = e
	c.sections[e.Path] = doc
}

// prepare canonicalizes the path and derives what body match consults (tag
// and title terms, path demotion); every entry passes through here first.
func (e *Entry) prepare() {
	e.Path = store.CanonicalPath(e.Path)
	e.terms = termSet(append(append([]string(nil), e.Tags...), e.Title))
	e.demote = 1
	if slices.Contains(strings.Split(strings.Trim(e.Path, "/"), "/"), "journal") {
		e.demote = journalDemote
	}
}

// SetSections installs a prebuilt section index for a document, for stores
// that carry indexes across snapshots. nil removes the index.
func (c *Catalog) SetSections(docPath string, doc *DocSections) {
	path := store.CanonicalPath(docPath)
	c.mu.Lock()
	defer c.mu.Unlock()
	if doc == nil {
		delete(c.sections, path)
		return
	}
	c.sections[path] = doc
}

// Sections returns the section index of a document, or nil when none.
func (c *Catalog) Sections(docPath string) *DocSections {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sections[store.CanonicalPath(docPath)]
}

// Remove deletes the entry and section index for the given path, if present.
func (c *Catalog) Remove(docPath string) {
	path := store.CanonicalPath(docPath)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, path)
	delete(c.sections, path)
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
	// Match selects catalog mode (default) or body mode.
	Match Mode
}

// Lookup returns documents whose tags or title match at least one query term,
// satisfy every filter predicate, and fall under the scope, ordered by
// descending match count, then importance, then modification time, then path.
// The error is always nil; the signature exists for the LookupCatalog seam.
//
// The query "*" matches every catalogued document under the scope (filters
// still apply): with all match scores equal, the ordering reduces to
// importance — "the most important documents here" without guessing a
// subject. This is the whole-catalog view that universe browsers build on.
func (c *Catalog) Lookup(query string, opts Options) ([]Result, error) {
	switch opts.Match {
	case "", MatchCatalog:
	case MatchBody:
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.lookupBody(queryTerms(query), NormalizeScope(opts.Scope), opts), nil
	default:
		return nil, fmt.Errorf("unknown match mode %q", opts.Match)
	}
	matchAll := strings.TrimSpace(query) == "*"
	terms := Tokenize(query)
	if !matchAll && len(terms) == 0 {
		return nil, nil
	}
	scope := NormalizeScope(opts.Scope)

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

// Tokenize lowercases s and splits it into distinct whitespace-separated
// terms, first-seen order. Exported so LookupCatalog backends tokenize
// identically.
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

// CountTerms counts distinct terms in a query, stopping once the count
// exceeds limit so an oversized query never builds the whole set.
func CountTerms(s string, limit int) int {
	seen := make(map[string]bool)
	for f := range strings.FieldsSeq(strings.ToLower(s)) {
		if !seen[f] {
			seen[f] = true
			if len(seen) > limit {
				break
			}
		}
	}
	return len(seen)
}

// MatchScore counts how many distinct query terms appear in the entry's tags
// (exact, case-insensitive) or title (substring, case-insensitive). Terms are
// normalized here; Lookup normalizes once via Tokenize and uses matchScore.
func MatchScore(e *Entry, terms []string) int {
	return matchScore(e, normalizeTerms(terms))
}

// normalizeTerms lowercases, trims, drops empties, and dedupes in order.
func normalizeTerms(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" || seen[term] {
			continue
		}
		seen[term] = true
		out = append(out, term)
	}
	return out
}

// matchScore scores against already-normalized terms.
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

// NormalizeScope returns a leading-slashed, trailing-slash-trimmed scope, or
// "/" for the whole-server scope. Exported so every LookupCatalog backend
// scopes identically; it deliberately does not clean the path.
func NormalizeScope(s string) string {
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
