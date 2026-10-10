package lookupexpand

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
)

var table = render.LookupResponse("text", "/", []render.LookupRow{
	{Path: "/a.md", Anchor: "two", Importance: 0.9, Title: "A › Two", Tags: []string{"a"}, Snippet: "snippet"},
	{Path: "/b.md", Importance: 0.8, Title: "B", Tags: []string{"b"}},
	{Path: "/a.md", Anchor: "three", Importance: 0.7, Title: "A › Three", Tags: []string{"a"}, Snippet: "snippet"},
	{Path: "/missing.md", Anchor: "x", Importance: 0.5, Title: "M", Tags: []string{"m"}},
}, protocol.MatchBody).Body

var docs = map[string]string{
	"/a.md": "# A\n\nIntro.\n\n## One\n\none text\n\n## Two\n\ntwo text\n\n## Three\n\n" + strings.Repeat("three text ", 30) + "\n",
	"/b.md": "# B\n\nShort whole document.\n",
}

// metadata is what fakeFetch reports beside a body, by path.
var metadata = map[string]map[string]string{}

// fakeFetch serves docs and records every path asked for.
func fakeFetch(calls *[]string) Fetch {
	return func(_ context.Context, path string) (Document, error) {
		*calls = append(*calls, path)
		body, ok := docs[path]
		if !ok {
			return Document{}, errors.New("not-found")
		}
		return Document{Body: body, Metadata: metadata[path]}, nil
	}
}

// expand runs Expand with nothing spent ahead of it.
func expand(ctx context.Context, table, query string, budget int, fetch Fetch) string {
	return Expand(ctx, Input{Table: table, Query: query, Budget: budget, Fetch: fetch})
}

// rowTable is the server's catalog table for the given locations.
func rowTable(locations ...string) string {
	rows := make([]render.LookupRow, 0, len(locations))
	for _, location := range locations {
		docPath, anchor, _ := strings.Cut(location, "#")
		rows = append(rows, render.LookupRow{Path: docPath, Anchor: anchor, Importance: 0.9, Title: "T", Tags: []string{"t"}})
	}
	return render.LookupResponse("q", "/", rows, "").Body
}

func TestExpandSectionsInOrderWithinBudget(t *testing.T) {
	var calls []string
	out := expand(context.Background(), table, "text", 10*1024, fakeFetch(&calls))
	for _, want := range []string{">>> /a.md#two\n\n## Two\n\ntwo text", ">>> /b.md#b\n\n# B\n\nShort whole document.", ">>> /a.md#three\n\n## Three", ">>> note: /missing.md: not-found", ">>> note: expanded 3 of 4 rows"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, ">>> /a.md#two") > strings.Index(out, ">>> /b.md") || strings.Index(out, ">>> /b.md") > strings.Index(out, ">>> /a.md#three") {
		t.Errorf("sections out of rank order:\n%s", out)
	}
	if len(calls) != 3 { // a.md once, b.md once, missing once
		t.Errorf("want one fetch per document, got %v", calls)
	}
}

func TestExpandSkipsSectionsThatDoNotFitAndNamesThem(t *testing.T) {
	var calls []string
	two := ">>> /a.md#two\n\n## Two\n\ntwo text\n\n"
	b := ">>> /b.md#b\n\n# B\n\nShort whole document.\n\n"
	out := expand(context.Background(), table, "text", len(two)+len(b)+40, fakeFetch(&calls))
	if !strings.Contains(out, two) || !strings.Contains(out, b) {
		t.Errorf("want the first two sections:\n%s", out)
	}
	if strings.Contains(out, ">>> /a.md#three\n") {
		t.Errorf("third section should not fit:\n%s", out)
	}
	for _, want := range []string{">>> note: did not fit: /a.md#three (~", ">>> note: expanded 2 of 4 rows"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// The budget is for the whole result: what the surface printed ahead of the
// expansion is spent, and a table that spends it all says so.
func TestExpandCountsTheTableAgainstTheBudget(t *testing.T) {
	var calls []string
	two := ">>> /a.md#two\n\n## Two\n\ntwo text\n\n"
	out := Expand(context.Background(), Input{Table: table, Query: "text", Budget: 400 + len(two) + 20, Spent: 400, Fetch: fakeFetch(&calls)})
	if !strings.Contains(out, two) || strings.Contains(out, ">>> /b.md#b\n") {
		t.Errorf("want only what the remainder holds:\n%s", out)
	}
	calls = nil
	out = Expand(context.Background(), Input{Table: table, Query: "text", Budget: 4000, Spent: 4000, Fetch: fakeFetch(&calls)})
	if len(calls) != 0 || !strings.Contains(out, ">>> note: the table alone used 1000 of 1000 result tokens; raise budget or fetch a row") {
		t.Errorf("want no fetches and the overspend note, got %d fetches:\n%s", len(calls), out)
	}
}

func TestExpandFitsATinySectionInASmallRemainder(t *testing.T) {
	docs["/t.md"] = "# T\n\n## One\n\n" + strings.Repeat("x", 300) + "\n\n## Two\n\nok\n"
	defer delete(docs, "/t.md")
	var calls []string
	tiny := ">>> /t.md#two\n\n## Two\n\nok\n\n"
	// Budget holds the tiny section but not the 300-byte one ahead of it.
	out := expand(context.Background(), rowTable("/t.md#one", "/t.md#two"), "x", len(tiny)+5, fakeFetch(&calls))
	if !strings.Contains(out, tiny) || strings.Contains(out, ">>> /t.md#one\n") {
		t.Errorf("want only the tiny section, decided on its own size:\n%s", out)
	}
}

// Nested rows on one document print once: a later row inside a printed
// section, or enclosing one, is reported rather than repeated.
func TestExpandCoalescesNestedSections(t *testing.T) {
	docs["/n.md"] = "# N\n\n## Two\n\ntwo text\n\n### Two A\n\ndeeper\n\n## Three\n\nthree\n"
	defer delete(docs, "/n.md")
	var calls []string
	out := expand(context.Background(), rowTable("/n.md#two", "/n.md#two-a", "/n.md#three"), "x", 4096, fakeFetch(&calls))
	if strings.Count(out, "deeper") != 1 || !strings.Contains(out, ">>> note: /n.md#two-a is within /n.md#two above") || !strings.Contains(out, ">>> /n.md#three\n") {
		t.Errorf("want the child reported, not reprinted, and the sibling printed:\n%s", out)
	}
	out = expand(context.Background(), rowTable("/n.md#two-a", "/n.md#two"), "x", 4096, fakeFetch(&calls))
	if strings.Count(out, "deeper") != 1 || !strings.Contains(out, ">>> note: /n.md#two encloses /n.md#two-a above; skipped") || !strings.Contains(out, "expanded 1 of 2 rows") {
		t.Errorf("want the stronger child kept and the parent reported:\n%s", out)
	}
}

// Identical text from a second location prints as a stub naming the first,
// so both citations stay available without a second copy.
func TestExpandCollapsesIdenticalText(t *testing.T) {
	docs["/copy.md"] = "# Copy\n\n## Two\n\ntwo text\n"
	defer delete(docs, "/copy.md")
	var calls []string
	out := expand(context.Background(), rowTable("/a.md#two", "/copy.md#two"), "x", 4096, fakeFetch(&calls))
	if strings.Count(out, "two text") != 1 || !strings.Contains(out, ">>> /copy.md#two\n\nsame text as /a.md#two\n\n") || !strings.Contains(out, "expanded 2 of 2 rows") {
		t.Errorf("want one copy and a stub:\n%s", out)
	}
}

// Relations declared by the strongest match print as lines and expand one
// hop within the same authority; the hop prints its own lines only.
func TestExpandFollowsTypedRelationsOneHop(t *testing.T) {
	docs["/adr/0006.md"] = "# ADR 0006\n\nOptional build.\n"
	docs["/adr/0010.md"] = "# ADR 0010\n\nRemoved.\n"
	metadata["/adr/0006.md"] = map[string]string{"rel-superseded-by": "/adr/0010.md", "rel-related": "/adr/0007.md"}
	metadata["/adr/0010.md"] = map[string]string{"rel-supersedes": "/adr/0006.md", "rel-depends-on": "mark://other/x.md"}
	defer func() {
		delete(docs, "/adr/0006.md")
		delete(docs, "/adr/0010.md")
		delete(metadata, "/adr/0006.md")
		delete(metadata, "/adr/0010.md")
	}()
	var calls []string
	out := expand(context.Background(), rowTable("/adr/0006.md"), "postgres", 4096, fakeFetch(&calls))
	for _, want := range []string{
		">>> /adr/0006.md#adr-0006\n\n# ADR 0006\n\nOptional build.\n\n>>> related: rel-related /adr/0007.md; rel-superseded-by /adr/0010.md\n\n",
		">>> /adr/0010.md#adr-0010\n\n# ADR 0010\n\nRemoved.\n\n>>> related: rel-depends-on mark://other/x.md; rel-supersedes /adr/0006.md\n\n",
		">>> note: expanded 1 of 1 rows and 1 related documents within the budget",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Every relation of the strongest match is followed, evidence predicates
	// first; the hop never fetches its source again or a foreign authority.
	if strings.Count(out, "Optional build.") != 1 || len(calls) != 3 || calls[1] != "/adr/0010.md" || calls[2] != "/adr/0007.md" || !strings.Contains(out, ">>> note: /adr/0007.md: not-found") {
		t.Errorf("want the source once, the typed hop first, then the related one, got %v:\n%s", calls, out)
	}
}

// A relation value with a fragment expands that section alone.
func TestExpandRelatedFragmentExpandsTheSection(t *testing.T) {
	docs["/impl.md"] = "# Impl\n\nBody.\n"
	metadata["/impl.md"] = map[string]string{"rel-implements": "/a.md#two"}
	defer func() { delete(docs, "/impl.md"); delete(metadata, "/impl.md") }()
	var calls []string
	out := expand(context.Background(), rowTable("/impl.md"), "x", 4096, fakeFetch(&calls))
	if !strings.Contains(out, ">>> /a.md#two\n\n## Two\n\ntwo text\n\n") || strings.Contains(out, "one text") {
		t.Errorf("want only the named section of the related document:\n%s", out)
	}
}

// Related expansion is bounded: MaxRelated documents, no table rows, no
// foreign authorities, and fetches count toward MaxFetches.
func TestExpandRelatedIsBounded(t *testing.T) {
	targets := make([]string, 0, MaxRelated+2)
	for i := range MaxRelated + 2 {
		path := fmt.Sprintf("/dep-%d.md", i)
		docs[path] = fmt.Sprintf("# Dep %d\n\nbody %d\n", i, i)
		targets = append(targets, path)
	}
	docs["/root.md"] = "# Root\n\nroot.\n"
	metadata["/root.md"] = map[string]string{"rel-depends-on": strings.Join(append([]string{"/b.md", "mark://else/y.md"}, targets...), ", ")}
	t.Cleanup(func() {
		for _, path := range append(targets, "/root.md") {
			delete(docs, path)
		}
		delete(metadata, "/root.md")
	})
	var calls []string
	out := expand(context.Background(), rowTable("/root.md", "/b.md"), "x", 1<<20, fakeFetch(&calls))
	if strings.Count(out, ">>> /dep-") != MaxRelated || strings.Count(out, ">>> /b.md#b\n") != 1 || strings.Contains(out, ">>> mark://else/y.md") {
		t.Errorf("want exactly %d related blocks, none for a table row or a foreign document:\n%s", MaxRelated, out)
	}
	if !strings.Contains(out, ", mark://else/y.md") || !strings.Contains(out, fmt.Sprintf("and %d related documents", MaxRelated)) {
		t.Errorf("want the foreign relation as a line and the count:\n%s", out)
	}
	if len(calls) != 2+MaxRelated { // root, b, then MaxRelated hops
		t.Errorf("fetches = %v", calls)
	}
}

func TestExpandOutlinesLargeBarePath(t *testing.T) {
	big := "# Big\n\nIntro.\n\n" + strings.Repeat("filler line\n\n", 800) + "## Tail\n\nend\n"
	docs["/big.md"] = big
	defer delete(docs, "/big.md")
	if len(big) < mdoutline.OutlineThreshold {
		t.Fatal("fixture must be over the threshold")
	}
	var calls []string
	out := expand(context.Background(), rowTable("/big.md"), "nothing matches", 10*1024, fakeFetch(&calls))
	if !strings.Contains(out, ">>> /big.md\n\n- Big (#big") || strings.Contains(out, "filler line\nfiller") {
		t.Errorf("want an outline, not the body:\n%s", out[:min(len(out), 400)])
	}
	out = expand(context.Background(), rowTable("/big.md"), "Tail end", 10*1024, fakeFetch(&calls))
	if !strings.Contains(out, ">>> /big.md#tail\n\n## Tail\n\nend") || strings.Contains(out, "(#big") {
		t.Errorf("want the term-matching section, not the outline:\n%s", out[:min(len(out), 400)])
	}
}

func TestExpandBoundsFetchesAndHonorsCancel(t *testing.T) {
	paths := make([]string, 0, MaxFetches+3)
	for i := range MaxFetches + 3 {
		paths = append(paths, fmt.Sprintf("/none-%d.md", i))
	}
	var calls []string
	out := expand(context.Background(), rowTable(paths...), "x", 1<<20, fakeFetch(&calls))
	if len(calls) != MaxFetches || !strings.Contains(out, fmt.Sprintf(">>> note: fetch limit of %d documents reached", MaxFetches)) {
		t.Errorf("want %d fetches then a note, got %d:\n%s", MaxFetches, len(calls), out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = nil
	out = expand(ctx, table, "x", 1<<20, fakeFetch(&calls))
	if len(calls) != 0 || !strings.Contains(out, ">>> note: stopped: context canceled") {
		t.Errorf("want no fetches after cancel, got %d:\n%s", len(calls), out)
	}
}

func TestExpandEscapesFrameLinesInBodies(t *testing.T) {
	docs["/q.md"] = "# Q\n\n## Quote\n\n>>> quoted three deep\n\nafter\n"
	defer delete(docs, "/q.md")
	var calls []string
	out := expand(context.Background(), rowTable("/q.md#quote"), "x", 4096, fakeFetch(&calls))
	if !strings.Contains(out, "\n >>> quoted three deep\n") || strings.Count(out, "\n>>> ") != 2 {
		t.Errorf("body frame line must be indented, only the header and the note may start with the delimiter:\n%s", out)
	}
}

func TestExpandFetchLimitKeepsCachedRows(t *testing.T) {
	paths := make([]string, 0, MaxFetches+2)
	for i := range MaxFetches {
		paths = append(paths, fmt.Sprintf("/none-%d.md", i))
	}
	paths = append(paths, "/b.md", "/a.md#two") // b.md needs a fetch past the limit; a.md is not cached either
	docs["/none-0.md"] = "# N0\n\nzero.\n"
	defer delete(docs, "/none-0.md")
	paths = append(paths, "/none-0.md")
	var calls []string
	out := expand(context.Background(), rowTable(paths...), "x", 1<<20, fakeFetch(&calls))
	if len(calls) != MaxFetches || !strings.Contains(out, "later rows on unfetched documents skipped") {
		t.Errorf("want %d fetches and one limit note, got %d:\n%s", MaxFetches, len(calls), out)
	}
	if !strings.Contains(out, ">>> /none-0.md#n0\n\n# N0") || strings.Contains(out, ">>> /b.md") {
		t.Errorf("a later row on a cached document must still expand, an unfetched one must not:\n%s", out)
	}
}

func TestExpandMissingSectionAndEmptyInputs(t *testing.T) {
	var calls []string
	out := expand(context.Background(), rowTable("/a.md#nine"), "x", 4096, fakeFetch(&calls))
	if !strings.Contains(out, ">>> note: /a.md: section #nine not found") {
		t.Errorf("want a missing-section note:\n%s", out)
	}
	if expand(context.Background(), table, "x", 0, fakeFetch(&calls)) != "" || expand(context.Background(), "no rows here", "x", 4096, fakeFetch(&calls)) != "" {
		t.Error("zero budget or no rows must expand to nothing")
	}
}

// A fetch that reports its version pins every block header to it.
func TestExpandPinsBlocksToTheFetchedVersion(t *testing.T) {
	fetch := func(_ context.Context, path string) (Document, error) {
		return Document{Body: docs[path], Version: 3}, nil
	}
	out := expand(context.Background(), rowTable("/a.md#two", "/b.md"), "two", 4096, fetch)
	for _, want := range []string{">>> /a.md/v3#two\n", ">>> /b.md/v3#b\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSameAuthority(t *testing.T) {
	for _, tc := range []struct {
		source, target string
		want           bool
	}{
		{"/a.md", "/b.md", true},
		{"/a.md", "mark://w/b.md", false},
		{"mark://w/a.md", "mark://w/b.md", true},
		{"mark://w/a.md", "mark://v/b.md", false},
		{"mark://w/a.md", "/b.md", false},
		{"/a.md", "b.md", false},
	} {
		if got := sameAuthority(tc.source, tc.target); got != tc.want {
			t.Errorf("sameAuthority(%q, %q) = %v, want %v", tc.source, tc.target, got, tc.want)
		}
	}
}

// Only the strongest match's relations expand; later rows print theirs as lines.
func TestExpandRelatedComesFromTheStrongestMatchOnly(t *testing.T) {
	docs["/first.md"] = "# First\n\nfirst.\n"
	docs["/second.md"] = "# Second\n\nsecond.\n"
	docs["/via-first.md"] = "# Via first\n\nvia first.\n"
	docs["/via-second.md"] = "# Via second\n\nvia second.\n"
	metadata["/first.md"] = map[string]string{"rel-genre": "/via-first.md"}
	metadata["/second.md"] = map[string]string{"rel-supersedes": "/via-second.md"}
	t.Cleanup(func() {
		for _, path := range []string{"/first.md", "/second.md", "/via-first.md", "/via-second.md"} {
			delete(docs, path)
			delete(metadata, path)
		}
	})
	var calls []string
	out := expand(context.Background(), rowTable("/first.md", "/second.md"), "x", 1<<20, fakeFetch(&calls))
	if !strings.Contains(out, ">>> /via-first.md#via-first\n") || strings.Contains(out, ">>> /via-second.md") || !strings.Contains(out, ">>> related: rel-supersedes /via-second.md\n") {
		t.Errorf("want the first row's hop only, the second row's relation as a line:\n%s", out)
	}
	if len(calls) != 3 {
		t.Errorf("fetches = %v", calls)
	}
}

// Evidence-bearing predicates are followed before navigation ones when the
// strongest match declares more than MaxRelated.
func TestExpandRelatedPrefersEvidencePredicates(t *testing.T) {
	meta := map[string]string{"rel-genre": "/g.md", "rel-by": "/by.md", "rel-label": "/l.md", "rel-supersedes": "/s.md"}
	docs["/top.md"] = "# Top\n\ntop.\n"
	metadata["/top.md"] = meta
	for _, path := range []string{"/g.md", "/by.md", "/l.md", "/s.md"} {
		docs[path] = "# " + path + "\n\nbody.\n"
	}
	t.Cleanup(func() {
		for _, path := range []string{"/top.md", "/g.md", "/by.md", "/l.md", "/s.md"} {
			delete(docs, path)
		}
		delete(metadata, "/top.md")
	})
	var calls []string
	out := expand(context.Background(), rowTable("/top.md"), "x", 1<<20, fakeFetch(&calls))
	if !strings.Contains(out, ">>> /s.md#smd\n") || strings.Count(out, ">>> /") != 1+MaxRelated || calls[1] != "/s.md" {
		t.Errorf("want the superseding document first among %d hops, got %v:\n%s", MaxRelated, calls, out)
	}
}

// A whole document opened by its H1 is pinned to that anchor; one with a
// preamble or a second H1 stays a whole-document block.
func TestExpandPinsWholeDocumentsToTheirOpeningHeading(t *testing.T) {
	docs["/pre.md"] = "preamble\n\n# Pre\n\nbody\n"
	docs["/two.md"] = "# One\n\nfirst\n\n# Two\n\nsecond\n"
	defer func() { delete(docs, "/pre.md"); delete(docs, "/two.md") }()
	var calls []string
	out := expand(context.Background(), rowTable("/b.md", "/pre.md", "/two.md"), "x", 1<<20, fakeFetch(&calls))
	for _, want := range []string{">>> /b.md#b\n", ">>> /pre.md\n", ">>> /two.md\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
