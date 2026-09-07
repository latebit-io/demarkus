package storetest

import (
	"sort"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// bodyLookupSubtests are the body-match cases RunLookupConformance appends
// to the catalog cases. Recall is the contract; order is asserted only where
// the spec fixes it (the importance prior, the path tiebreak).
var bodyLookupSubtests = []lookupSubtest{
	{"BodyRecallOverSections", testBodyRecallOverSections},
	{"BodyHeadinglessDocument", testBodyHeadinglessDocument},
	{"BodyImportancePrior", testBodyImportancePrior},
	{"BodyLimit", testBodyLimit},
	{"BodyUnknownModeRejected", testBodyUnknownModeRejected},
	{"BodyScopeAndFilter", testBodyScopeAndFilter},
	{"BodyArchiveAndUpdate", testBodyArchiveAndUpdate},
	{"BodyCatalogModeUnchanged", testBodyCatalogModeUnchanged},
}

// debuggingDoc has a term in every position the match rule names: own text,
// a subsection, a heading, a heading trail, tags, and the title.
const debuggingDoc = "# Debugging\n\nIntro line about udp buffers.\n\n" +
	"## QUIC UDP Buffer Size Warning\n\nquic-go tries to increase the UDP receive buffer to 7168 kiB.\nRestricted sysctl limits block it.\n\n" +
	"### Workaround\n\nSet `net.core.rmem_max` and retry.\n\n" +
	"## Poison Lock\n\nIgnoring the lock pid write creates a poison lock; `path.Match` is not the culprit.\n"

func bodyOpts() catalog.Options { return catalog.Options{Match: catalog.MatchBody} }

func testBodyRecallOverSections(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/debugging.md", debuggingDoc, map[string]string{"tags": "debugging,gotchas", "importance": "0.7"})
	catalogPublish(t, b, "/other.md", "# Other\n\nNothing to see here.\n", map[string]string{"tags": "misc"})

	quic := "/debugging.md#quic-udp-buffer-size-warning"
	workaround := "/debugging.md#workaround"
	// Own text and heading trail: "buffers" in the H1 text is not the term
	// "buffer", and Workaround inherits its parent heading's words.
	assertBodyRows(t, b, "udp buffer", bodyOpts(), quic, workaround)
	assertBodyRows(t, b, "sysctl", bodyOpts(), quic)
	assertBodyRows(t, b, "SYSCTL", bodyOpts(), quic)
	// Every term must appear: no section holds both.
	assertBodyRows(t, b, "udp poison", bodyOpts())
	// Heading text and the heading trail count.
	assertBodyRows(t, b, "warning", bodyOpts(), quic, workaround)
	assertBodyRows(t, b, "workaround quic", bodyOpts(), workaround)
	// Tags and title satisfy a term for every section, so only the other
	// terms must be in a section's own text or trail. A document whose tags
	// or title carry every term is one bare-path row: the catalog answer.
	assertBodyRows(t, b, "gotchas sysctl", bodyOpts(), quic)
	assertBodyRows(t, b, "debugging sysctl", bodyOpts(), quic)
	assertBodyRows(t, b, "gotchas", bodyOpts(), "/debugging.md")
	assertBodyRows(t, b, "debugging gotchas", bodyOpts(), "/debugging.md")
	assertBodyRows(t, b, "gotchas misc", bodyOpts())
	// Sub-token and whole-word forms both match; underscore is a word char.
	assertBodyRows(t, b, "path.Match", bodyOpts(), "/debugging.md#poison-lock")
	assertBodyRows(t, b, "match", bodyOpts(), "/debugging.md#poison-lock")
	assertBodyRows(t, b, "rmem_max", bodyOpts(), "/debugging.md#workaround")
	// Terms under two characters are dropped; nothing usable matches nothing.
	assertBodyRows(t, b, "a b", bodyOpts())
	assertBodyRows(t, b, "a sysctl", bodyOpts(), quic)

	rs := mustLookup(t, b, "sysctl", bodyOpts())
	if len(rs) != 1 {
		t.Fatalf("rows = %d, want 1", len(rs))
	}
	r := rs[0]
	if r.Path != "/debugging.md" || r.Anchor != "quic-udp-buffer-size-warning" || r.Heading != "QUIC UDP Buffer Size Warning" {
		t.Errorf("row = %+v, want path, anchor, heading of the QUIC section", r)
	}
	if r.Title != "Debugging" || r.Importance != 0.7 || len(r.Tags) != 2 {
		t.Errorf("row carries entry fields %q %g %v, want Debugging 0.7 [debugging gotchas]", r.Title, r.Importance, r.Tags)
	}
	assertSnippet(t, r.Snippet, "sysctl")
}

func testBodyHeadinglessDocument(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/plain.md", "just a paragraph mentioning hairpin nat\n", map[string]string{"title": "Plain"})
	catalogPublish(t, b, "/pre.md", "Preamble mentions kqueue.\n\n# Pre Title\n\nbody text\n", nil)

	assertBodyRows(t, b, "hairpin", bodyOpts(), "/plain.md")
	assertBodyRows(t, b, "kqueue", bodyOpts(), "/pre.md")
	assertBodyRows(t, b, "text", bodyOpts(), "/pre.md#pre-title")
	rs := mustLookup(t, b, "hairpin", bodyOpts())
	if len(rs) != 1 || rs[0].Anchor != "" || rs[0].Heading != "" || rs[0].Title != "Plain" {
		t.Errorf("bare-path row = %+v, want empty anchor and heading, title Plain", rs)
	}
	assertSnippet(t, rs[0].Snippet, "hairpin")
}

func testBodyImportancePrior(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/lo.md", "# Lo\n\nshared phrase apple\n", map[string]string{"importance": "0.2"})
	catalogPublish(t, b, "/hi.md", "# Hi\n\nshared phrase apple\n", map[string]string{"importance": "0.9"})
	// Highest importance of all, but never matches: must not appear.
	catalogPublish(t, b, "/top.md", "# Top\n\nbanana\n", map[string]string{"importance": "1"})
	// Equal text and importance: ascending path breaks the tie.
	catalogPublish(t, b, "/tie/b.md", "# B\n\npear\n", map[string]string{"importance": "0.5"})
	catalogPublish(t, b, "/tie/a.md", "# A\n\npear\n", map[string]string{"importance": "0.5"})

	assertBodyOrder(t, b, "apple", bodyOpts(), "/hi.md#hi", "/lo.md#lo")
	assertBodyOrder(t, b, "pear", bodyOpts(), "/tie/a.md#a", "/tie/b.md#b")
}

func testBodyLimit(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/m1.md", "# One\n\ncherry\n", map[string]string{"importance": "0.9"})
	catalogPublish(t, b, "/m2.md", "# Two\n\ncherry\n", map[string]string{"importance": "0.7"})
	catalogPublish(t, b, "/m3.md", "# Three\n\ncherry\n\n## More\n\ncherry again\n", map[string]string{"importance": "0.5"})

	all := mustLookup(t, b, "cherry", bodyOpts())
	if len(all) != 4 {
		t.Fatalf("uncapped rows = %d, want 4 (one per section)", len(all))
	}
	capped := mustLookup(t, b, "cherry", catalog.Options{Match: catalog.MatchBody, Max: 2})
	if len(capped) != 2 || rowKey(&capped[0]) != rowKey(&all[0]) || rowKey(&capped[1]) != rowKey(&all[1]) {
		t.Errorf("capped rows = %v, want the first two of %v", rowKeys(capped), rowKeys(all))
	}
}

func testBodyUnknownModeRejected(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/a.md", "# A\n\nfig\n", map[string]string{"tags": "go"})
	if _, err := b.Catalog.Lookup("go", catalog.Options{Match: "bogus"}); err == nil {
		t.Error("unknown mode accepted, want error")
	}
	// The empty mode and the explicit catalog mode are the same lookup.
	assertLookup(t, b, "go", catalog.Options{}, "/a.md")
	assertLookup(t, b, "go", catalog.Options{Match: catalog.MatchCatalog}, "/a.md")
}

func testBodyScopeAndFilter(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/docs/a.md", "# A\n\ngrape\n", map[string]string{"project": "broker"})
	catalogPublish(t, b, "/other/b.md", "# B\n\ngrape\n", map[string]string{"project": "universe"})

	assertBodyRows(t, b, "grape", bodyOpts(), "/docs/a.md#a", "/other/b.md#b")
	assertBodyRows(t, b, "grape", catalog.Options{Match: catalog.MatchBody, Scope: "/docs/"}, "/docs/a.md#a")
	assertBodyRows(t, b, "grape", catalog.Options{Match: catalog.MatchBody, Scope: "/oth"})
	assertBodyRows(t, b, "grape", catalog.Options{Match: catalog.MatchBody, Filter: mustParseFilter(t, "project=universe")}, "/other/b.md#b")
	assertBodyRows(t, b, "grape", catalog.Options{Match: catalog.MatchBody, Filter: mustParseFilter(t, "modified-after=2100-01-01")})
}

func testBodyArchiveAndUpdate(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/doc.md", "# Doc\n\nlemon\n\n## Old\n\nlime\n", nil)
	assertBodyRows(t, b, "lime", bodyOpts(), "/doc.md#old")

	catalogArchive(t, b, "/doc.md")
	assertBodyRows(t, b, "lime", bodyOpts())
	catalogUnarchive(t, b, "/doc.md")
	assertBodyRows(t, b, "lime", bodyOpts(), "/doc.md#old")

	// A new version replaces the old sections wholesale.
	catalogPublish(t, b, "/doc.md", "# Doc\n\n## New\n\nmelon\n", nil)
	assertBodyRows(t, b, "lime", bodyOpts())
	assertBodyRows(t, b, "lemon", bodyOpts())
	assertBodyRows(t, b, "melon", bodyOpts(), "/doc.md#new")
}

func testBodyCatalogModeUnchanged(t *testing.T, b LookupBackend) {
	catalogPublish(t, b, "/doc.md", "# Doc\n\n## Section\n\npapaya\n", map[string]string{"tags": "fruit"})

	// Catalog mode never sees the body.
	assertLookup(t, b, "papaya", catalog.Options{})
	assertLookup(t, b, "papaya", catalog.Options{Match: catalog.MatchCatalog})
	rs := mustLookup(t, b, "fruit", catalog.Options{Match: catalog.MatchCatalog})
	if len(rs) != 1 || rs[0].Anchor != "" || rs[0].Heading != "" || rs[0].Snippet != "" {
		t.Errorf("catalog row = %+v, want no section fields", rs)
	}
}

// rowKey renders a body row as path#anchor, or the bare path.
func rowKey(r *catalog.Result) string {
	if r.Anchor == "" {
		return r.Path
	}
	return r.Path + "#" + r.Anchor
}

func rowKeys(rs []catalog.Result) []string {
	keys := make([]string, len(rs))
	for i := range rs {
		keys[i] = rowKey(&rs[i])
	}
	return keys
}

// assertBodyRows asserts the row set of a body lookup, ignoring order.
func assertBodyRows(t *testing.T, b LookupBackend, query string, opts catalog.Options, want ...string) {
	t.Helper()
	got := rowKeys(mustLookup(t, b, query, opts))
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Errorf("body lookup %q = %v, want %v", query, got, sorted)
	}
}

// assertBodyOrder asserts the exact ordered rows of a body lookup.
func assertBodyOrder(t *testing.T, b LookupBackend, query string, opts catalog.Options, want ...string) {
	t.Helper()
	got := rowKeys(mustLookup(t, b, query, opts))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("body lookup %q = %v, want %v", query, got, want)
	}
}

// assertSnippet checks the spec's snippet shape: one line, capped, and
// showing the matched term in this reference implementation.
func assertSnippet(t *testing.T, snippet, term string) {
	t.Helper()
	if snippet == "" || strings.ContainsAny(snippet, "\n\r|") || len(snippet) > catalog.SnippetBytes {
		t.Errorf("snippet %q: want one line under %d bytes without table characters", snippet, catalog.SnippetBytes)
	}
	if !strings.Contains(strings.ToLower(snippet), term) {
		t.Errorf("snippet %q does not show the matched term %q", snippet, term)
	}
}
