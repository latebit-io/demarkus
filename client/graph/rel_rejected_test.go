package graph

import (
	"strings"
	"testing"
)

// ADR 0004: a bad rel- value never fails a crawl, but it must not vanish.
func TestRelEdgesReportsRejectedValues(t *testing.T) {
	result := RelEdges("mark://host/doc.md", map[string]string{
		"rel-supersedes": "/old.md, two words.md, /doc.md",
		"rel-":           "/x.md",
		"rel-cites":      "/a.md,",
		"tags":           "not a relation",
	})

	if len(result.Refs) != 2 {
		t.Fatalf("refs = %+v, want /old.md and /a.md", result.Refs)
	}
	want := map[string]string{
		"rel-supersedes=two words.md": "whitespace",
		"rel-supersedes=/doc.md":      "self",
		"rel-=/x.md":                  "predicate",
	}
	if len(result.Rejected) != len(want) {
		t.Fatalf("rejected = %+v, want %d entries", result.Rejected, len(want))
	}
	for _, r := range result.Rejected {
		reason, ok := want[r.Key+"="+r.Value]
		if !ok {
			t.Errorf("unexpected rejection %+v", r)
			continue
		}
		if !strings.Contains(r.Reason, reason) {
			t.Errorf("reason for %s = %q, want it to mention %q", r.Value, r.Reason, reason)
		}
	}
}

func TestExtractDocumentEdgesCarriesRejectedRels(t *testing.T) {
	extracted := ExtractDocumentEdges("mark://host/doc.md", "", map[string]string{"rel-cites": "bad ref"})
	if len(extracted.Edges) != 0 || len(extracted.RejectedRels) != 1 {
		t.Errorf("extracted = %+v, want no edges and one rejection", extracted)
	}
}

func TestCrawlOutcomeSummaryCountsRejectedRels(t *testing.T) {
	o := &CrawlOutcome{Complete: true, RejectedRels: 2}
	if got := o.Summary(); !strings.Contains(got, "rejected relations: 2") {
		t.Errorf("summary = %q", got)
	}
	if got := (&CrawlOutcome{Complete: true}).Summary(); strings.Contains(got, "rejected") {
		t.Errorf("summary mentions rejections with none: %q", got)
	}
}
