package storetest

import (
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/store"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// LookupBackend pairs a DocumentStore with the LookupCatalog observing it:
// file store + in-memory catalog, or pgstore as both.
type LookupBackend struct {
	Store   handler.DocumentStore
	Catalog handler.LookupCatalog
	Views   storagebackend.ViewProvider
}

// LookupFactory returns a fresh, empty backend for one conformance subtest.
type LookupFactory func(t *testing.T) LookupBackend

// RunLookupConformance exercises the LookupCatalog contract: every backend
// must rank, score, scope, and filter identically. Helpers drive the catalog
// exactly as the handler's write paths do.
func RunLookupConformance(t *testing.T, factory LookupFactory) {
	subtests := []struct {
		name string
		fn   func(t *testing.T, b LookupBackend)
	}{
		{"Matching", testLookupMatching},
		{"Ranking", testLookupRanking},
		{"Tiebreaks", testLookupTiebreaks},
		{"TitleFallback", testLookupTitleFallback},
		{"WildcardLiteralTerms", testLookupWildcardLiteralTerms},
		{"MatchAll", testLookupMatchAll},
		{"Scope", testLookupScope},
		{"Filters", testLookupFilters},
		{"ArchiveLifecycle", testLookupArchiveLifecycle},
		{"UpdateReplacesEntry", testLookupUpdateReplacesEntry},
		{"MaxCap", testLookupMaxCap},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			st.fn(t, factory(t))
		})
	}
}

func testLookupMatching(t *testing.T, b LookupBackend) {
	// Distinct importances keep same-score orderings timestamp-independent.
	catalogPublish(t, b, "/tagged.md", "body", map[string]string{"tags": "auth,Go", "title": "Unrelated", "importance": "0.6"})
	catalogPublish(t, b, "/titled.md", "body", map[string]string{"tags": "misc", "title": "Auth Middleware Design"})
	catalogPublish(t, b, "/substring-tag.md", "body", map[string]string{"tags": "golang", "title": "Nothing here"})

	// Tag matches are exact (case-insensitive); title matches are substrings.
	assertLookup(t, b, "auth", catalog.Options{}, "/tagged.md", "/titled.md")
	assertLookup(t, b, "AUTH", catalog.Options{}, "/tagged.md", "/titled.md")
	assertLookup(t, b, "middleware", catalog.Options{}, "/titled.md")
	// "go" is not the tag "golang", and no title contains it.
	assertLookup(t, b, "go", catalog.Options{}, "/tagged.md")
	assertLookup(t, b, "kubernetes", catalog.Options{})
	assertLookup(t, b, "", catalog.Options{})
	assertLookup(t, b, "   ", catalog.Options{})

	// Entry contents survive the round trip: original-case tags, title, score.
	rs := mustLookup(t, b, "middleware", catalog.Options{})
	if len(rs) != 1 || rs[0].Title != "Auth Middleware Design" || rs[0].Score != 1 {
		t.Errorf("result = %+v, want title %q score 1", rs, "Auth Middleware Design")
	}
	rs = mustLookup(t, b, "auth", catalog.Options{})
	if len(rs) == 0 || len(rs[0].Tags) != 2 || rs[0].Tags[0] != "auth" || rs[0].Tags[1] != "Go" {
		t.Errorf("tags = %+v, want original-case [auth Go]", rs)
	}
}

func testLookupRanking(t *testing.T, b LookupBackend) {
	// Matches one term, high importance.
	catalogPublish(t, b, "/one-term.md", "x", map[string]string{"tags": "auth", "importance": "0.99"})
	// Matches both terms: score must beat importance.
	catalogPublish(t, b, "/two-terms.md", "x", map[string]string{"tags": "auth,middleware", "importance": "0.1"})
	// Same score as one-term, lower importance.
	catalogPublish(t, b, "/also-one.md", "x", map[string]string{"tags": "middleware", "importance": "0.5"})

	assertLookup(t, b, "auth middleware", catalog.Options{},
		"/two-terms.md", "/one-term.md", "/also-one.md")

	// Duplicate terms count once: score stays 2, not 3.
	rs := mustLookup(t, b, "auth auth middleware", catalog.Options{})
	if len(rs) == 0 || rs[0].Path != "/two-terms.md" || rs[0].Score != 2 {
		t.Errorf("distinct-term score = %+v, want /two-terms.md with score 2", rs)
	}
}

func testLookupTiebreaks(t *testing.T, b LookupBackend) {
	// Same score and importance: modified breaks the tie, then path asc.
	// Derive the expected order from stored timestamps; writes may straddle
	// a second boundary.
	catalogPublish(t, b, "/tie/b.md", "x", map[string]string{"tags": "go", "importance": "0.5"})
	catalogPublish(t, b, "/tie/a.md", "x", map[string]string{"tags": "go", "importance": "0.5"})

	first, second := "/tie/a.md", "/tie/b.md" // equal times: path ascending
	if modifiedOf(t, b, "/tie/a.md").Before(modifiedOf(t, b, "/tie/b.md")) {
		first, second = "/tie/b.md", "/tie/a.md" // b is newer: newest first
	}
	assertLookup(t, b, "go", catalog.Options{}, first, second)
}

func testLookupTitleFallback(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/declared.md", "# Ignored Heading\n", map[string]string{"title": "Declared Winner"})
	catalogPublish(t, b, "/heading.md", "# From The Heading\n\nbody\n", nil)
	catalogPublish(t, b, "/basename.md", "no heading at all\n", nil)

	assertLookup(t, b, "winner", catalog.Options{}, "/declared.md")
	// The declared title replaced the H1, so the H1 must not match.
	assertLookup(t, b, "ignored", catalog.Options{})
	assertLookup(t, b, "heading", catalog.Options{}, "/heading.md")
	assertLookup(t, b, "basename.md", catalog.Options{}, "/basename.md")
}

// testLookupWildcardLiteralTerms pins SQL-wildcard characters as literal
// substrings: an unescaped LIKE prefilter would match every title on "%"
// and "_", and nothing on "\".
func testLookupWildcardLiteralTerms(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/pct.md", "x", map[string]string{"title": "Coverage 100% done", "importance": "0.6"})
	catalogPublish(t, b, "/und.md", "x", map[string]string{"title": "snake_case naming", "importance": "0.5"})
	catalogPublish(t, b, "/bsl.md", "x", map[string]string{"title": `back\slash path`, "importance": "0.4"})
	catalogPublish(t, b, "/plain.md", "x", map[string]string{"title": "nothing special", "importance": "0.3"})

	assertLookup(t, b, "%", catalog.Options{}, "/pct.md")
	assertLookup(t, b, "100%", catalog.Options{}, "/pct.md")
	assertLookup(t, b, "_", catalog.Options{}, "/und.md")
	assertLookup(t, b, `\`, catalog.Options{}, "/bsl.md")
	assertLookup(t, b, `back\slash`, catalog.Options{}, "/bsl.md")
}

func testLookupMatchAll(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/low.md", "x", map[string]string{"tags": "go", "importance": "0.2"})
	catalogPublish(t, b, "/hub.md", "x", map[string]string{"tags": "hub", "importance": "0.9"})
	catalogPublish(t, b, "/docs/deep.md", "x", map[string]string{"tags": "go", "importance": "0.7"})

	// "*" returns everything, importance-ranked, all scores zero.
	assertLookup(t, b, "*", catalog.Options{}, "/hub.md", "/docs/deep.md", "/low.md")
	rs := mustLookup(t, b, "*", catalog.Options{})
	for _, r := range rs {
		if r.Score != 0 {
			t.Errorf("match-all score for %s = %d, want 0", r.Path, r.Score)
		}
	}
	// Scope, filters, and Max still narrow the universe.
	assertLookup(t, b, "*", catalog.Options{Scope: "/docs/"}, "/docs/deep.md")
	preds := mustParseFilter(t, "tag=go")
	assertLookup(t, b, "*", catalog.Options{Filter: preds}, "/docs/deep.md", "/low.md")
	assertLookup(t, b, "*", catalog.Options{Max: 1}, "/hub.md")
	// A star mixed with other terms is a normal term query, not match-all.
	assertLookup(t, b, "* hub", catalog.Options{}, "/hub.md")
}

func testLookupScope(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/docs/a.md", "x", map[string]string{"tags": "go", "importance": "0.9"})
	catalogPublish(t, b, "/docs/sub/b.md", "x", map[string]string{"tags": "go", "importance": "0.4"})
	catalogPublish(t, b, "/other/c.md", "x", map[string]string{"tags": "go", "importance": "0.6"})

	all := []string{"/docs/a.md", "/other/c.md", "/docs/sub/b.md"} // 0.9, 0.6, 0.4
	assertLookup(t, b, "go", catalog.Options{Scope: "/"}, all...)
	assertLookup(t, b, "go", catalog.Options{Scope: ""}, all...)
	assertLookup(t, b, "go", catalog.Options{Scope: "/docs/"}, "/docs/a.md", "/docs/sub/b.md")
	assertLookup(t, b, "go", catalog.Options{Scope: "/docs"}, "/docs/a.md", "/docs/sub/b.md")
	assertLookup(t, b, "go", catalog.Options{Scope: "/docs/sub"}, "/docs/sub/b.md")
	// A raw prefix must not leak across path segments.
	assertLookup(t, b, "go", catalog.Options{Scope: "/oth"})
}

func testLookupFilters(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/broker.md", "x", map[string]string{"tags": "go", "project": "broker"})
	catalogPublish(t, b, "/universe.md", "x", map[string]string{"tags": "go,Design", "project": "universe"})

	assertLookup(t, b, "go", catalog.Options{Filter: mustParseFilter(t, "project=broker")}, "/broker.md")
	// Tag membership filter is case-insensitive.
	assertLookup(t, b, "go", catalog.Options{Filter: mustParseFilter(t, "tag=design")}, "/universe.md")
	// Both documents were written after 2000 and before 2100.
	assertLookup(t, b, "go", catalog.Options{Filter: mustParseFilter(t, "modified-after=2100-01-01")})
	assertLookup(t, b, "go", catalog.Options{Filter: mustParseFilter(t, "modified-before=2000-01-01")})
	got := mustLookup(t, b, "go", catalog.Options{Filter: mustParseFilter(t, "modified-after=2000-01-01,modified-before=2100-01-01")})
	if len(got) != 2 {
		t.Errorf("date-window filter matched %d docs, want 2", len(got))
	}
}

func testLookupArchiveLifecycle(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/keep.md", "x", map[string]string{"tags": "go"})
	catalogPublish(t, b, "/hide.md", "x", map[string]string{"tags": "go", "importance": "0.4"})

	assertLookup(t, b, "go", catalog.Options{}, "/keep.md", "/hide.md")
	catalogArchive(t, b, "/hide.md")
	assertLookup(t, b, "go", catalog.Options{}, "/keep.md")
	// Archived documents are invisible to match-all too.
	assertLookup(t, b, "*", catalog.Options{}, "/keep.md")
	catalogUnarchive(t, b, "/hide.md")
	assertLookup(t, b, "go", catalog.Options{}, "/keep.md", "/hide.md")
}

func testLookupUpdateReplacesEntry(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/doc.md", "x", map[string]string{"tags": "alpha", "importance": "0.3"})
	catalogPublish(t, b, "/doc.md", "y", map[string]string{"tags": "beta", "importance": "0.8"})

	assertLookup(t, b, "alpha", catalog.Options{})
	assertLookup(t, b, "beta", catalog.Options{}, "/doc.md")
	rs := mustLookup(t, b, "beta", catalog.Options{})
	if len(rs) != 1 || rs[0].Importance != 0.8 {
		t.Errorf("updated entry = %+v, want importance 0.8", rs)
	}
}

func testLookupMaxCap(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/m1.md", "x", map[string]string{"tags": "go", "importance": "0.9"})
	catalogPublish(t, b, "/m2.md", "x", map[string]string{"tags": "go", "importance": "0.7"})
	catalogPublish(t, b, "/m3.md", "x", map[string]string{"tags": "go", "importance": "0.5"})

	assertLookup(t, b, "go", catalog.Options{Max: 2}, "/m1.md", "/m2.md")
	assertLookup(t, b, "go", catalog.Options{Max: 0}, "/m1.md", "/m2.md", "/m3.md")
}

// FileBackend is the file store paired with the in-memory catalog: the
// reference backend every other one is compared against.
func FileBackend(t testing.TB) LookupBackend {
	documents := store.New(t.TempDir())
	wrapped := filestore.New(documents, catalog.New())
	return LookupBackend{Store: wrapped, Catalog: wrapped, Views: wrapped}
}

// The *Into helpers drive a backend exactly as the handler's write paths do
// (store op, then catalog maintenance) and return the store's error; the
// conformance and differential suites share them so they cannot drift.

func publishInto(b LookupBackend, path string, expected int, body []byte, meta map[string]string) (*store.Document, error) {
	doc, err := b.Store.WriteVersion(path, expected, body, meta)
	if err == nil {
		b.Catalog.Put(path, doc.Metadata, doc.Content, doc.Modified)
	}
	return doc, err
}

func appendInto(b LookupBackend, path string, expected int, body []byte, meta map[string]string) (*store.Document, error) {
	doc, err := b.Store.Append(path, expected, body, meta)
	if err == nil {
		b.Catalog.Put(path, doc.Metadata, doc.Content, doc.Modified)
	}
	return doc, err
}

func archiveInto(b LookupBackend, path string) error {
	if _, _, err := b.Store.Archive(path, true); err != nil {
		return err
	}
	b.Catalog.Remove(path)
	return nil
}

// unarchiveInto mirrors the handler's empty-body PUBLISH path.
func unarchiveInto(b LookupBackend, path string) error {
	doc, _, err := b.Store.Archive(path, false)
	if err != nil {
		return err
	}
	b.Catalog.Put(path, doc.Metadata, doc.Content, doc.Modified)
	return nil
}

func catalogPublish(t *testing.T, b LookupBackend, path, body string, meta map[string]string) {
	t.Helper()
	if _, err := publishInto(b, path, -1, []byte(body), meta); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func catalogArchive(t *testing.T, b LookupBackend, path string) {
	t.Helper()
	if err := archiveInto(b, path); err != nil {
		t.Fatalf("archive %s: %v", path, err)
	}
}

func catalogUnarchive(t *testing.T, b LookupBackend, path string) {
	t.Helper()
	if err := unarchiveInto(b, path); err != nil {
		t.Fatalf("unarchive %s: %v", path, err)
	}
}

// mustLookup runs a lookup, failing the test on error.
func mustLookup(t *testing.T, b LookupBackend, query string, opts catalog.Options) []catalog.Result {
	t.Helper()
	rs, err := b.Catalog.Lookup(query, opts)
	if err != nil {
		t.Fatalf("lookup %q: %v", query, err)
	}
	return rs
}

// assertLookup asserts the exact ordered result paths of a lookup.
func assertLookup(t *testing.T, b LookupBackend, query string, opts catalog.Options, want ...string) {
	t.Helper()
	rs := mustLookup(t, b, query, opts)
	got := make([]string, len(rs))
	for i, r := range rs {
		got[i] = r.Path
	}
	if len(got) != len(want) {
		t.Errorf("lookup %q = %v, want %v", query, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("lookup %q = %v, want %v", query, got, want)
			return
		}
	}
}

// mustParseFilter parses a filter expression, failing the test on error.
func mustParseFilter(t *testing.T, s string) []catalog.Predicate {
	t.Helper()
	preds, err := catalog.ParseFilter(s)
	if err != nil {
		t.Fatalf("parse filter %q: %v", s, err)
	}
	return preds
}

// modifiedOf returns a document's current modification time.
func modifiedOf(t *testing.T, b LookupBackend, path string) time.Time {
	t.Helper()
	doc, err := b.Store.Get(path, 0)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return doc.Modified
}
