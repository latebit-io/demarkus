package catalog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
)

func similarCatalog() *Catalog {
	c := New()
	meta := map[string]string{"tags": "store", "importance": "0.5"}
	putDoc(c, "/debugging.md", neardtest.HubDoc, meta)
	putDoc(c, "/notes/tenants.md", neardtest.OtherDoc, meta)
	putDoc(c, "/journal/2026-10-21.md", neardtest.LessonDoc, meta)
	return c
}

// similarTo is a similar-mode lookup for body under opts.
func similarTo(t *testing.T, c *Catalog, body string, opts Options) []Result {
	t.Helper()
	draft := neardup.AnalyzeWhole(body)
	opts.Match, opts.Draft = MatchSimilar, &draft
	return mustLookup(t, c, "", opts)
}

func TestSimilarFindsTheRewordedCopy(t *testing.T) {
	c := similarCatalog()
	hits := similarTo(t, c, neardtest.RewordedDoc, Options{})
	if len(hits) != 2 {
		t.Fatalf("hits = %+v, want the journal entry and the debugging section", hits)
	}
	if h := hits[0]; h.Path != "/journal/2026-10-21.md" || h.Hit.Verdict != neardup.VerdictNearDuplicate || h.Anchor != "" || h.Hit.Why() != "near-duplicate (0.76 shared)" {
		t.Errorf("first hit = %+v, want near-duplicate of the whole journal entry", h)
	}
	if h := hits[1]; h.Path != "/debugging.md" || h.Hit.Verdict != neardup.VerdictContained || h.Anchor != "shards" || h.DisplayTitle() != "Patterns › Shards" || h.Tags[0] != "store" {
		t.Errorf("second hit = %+v, want contained in the shards section with its heading", h)
	}
	if hits := similarTo(t, c, neardtest.RewordedDoc, Options{Scope: "/notes/"}); len(hits) != 0 {
		t.Errorf("hits under /notes/ = %+v, want none", hits)
	}
}

func TestSimilarSkips(t *testing.T) {
	c := similarCatalog()
	skipJournal := func(path string) bool { return strings.HasPrefix(path, "/journal/") }
	if hits := similarTo(t, c, neardtest.RewordedDoc, Options{Skip: skipJournal}); len(hits) != 1 || hits[0].Path != "/debugging.md" {
		t.Errorf("hits with journal skipped = %+v", hits)
	}
	if hits := similarTo(t, c, "# Short\n\ntoo short to sign\n", Options{}); hits != nil {
		t.Errorf("short draft hits = %+v", hits)
	}
	if hits := similarTo(t, c, neardtest.OtherDoc, Options{}); len(hits) != 2 {
		t.Errorf("tenant note hits = %+v, want the note and the debugging section", hits)
	}
	if hits := mustLookup(t, c, "", Options{Match: MatchSimilar}); hits != nil {
		t.Errorf("similar without a draft = %+v", hits)
	}
}

// A 1000-document world keeps its signatures at a few bytes per section;
// the scan itself retains nothing.
func TestSimilarRetainsNothing(t *testing.T) {
	c := New()
	for i := range 1000 {
		body := fmt.Sprintf("# Note %d\n\n## One\n\n%s\n\n## Two\n\nvariant %d %s\n", i, neardtest.Other, i, neardtest.Lesson)
		putDoc(c, fmt.Sprintf("/notes/%d.md", i), body, nil)
	}
	growth := memtest.Retained(func() {
		for range 20 {
			if hits := similarTo(t, c, neardtest.RewordedDoc, Options{}); len(hits) == 0 {
				t.Fatal("no hits over the 1000-document world")
			}
		}
	})
	if growth > 64<<10 {
		t.Errorf("heap grew %d bytes over 20 scans, want under 64 KB", growth)
	}
}
