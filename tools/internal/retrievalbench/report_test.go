package retrievalbench

import (
	"strings"
	"testing"
)

func TestSummarize(t *testing.T) {
	results := []QuestionResult{
		{ID: "v1", Category: CategoryVocabulary, Hit: true, CallsToEvidence: 2, CallsTotal: 2, Tokens: 100, ElapsedMS: 10},
		{ID: "v2", Category: CategoryVocabulary, Hit: false, CallsTotal: 6, Tokens: 900, ElapsedMS: 30},
		{ID: "b1", Category: CategoryBody, Hit: true, CallsToEvidence: 4, CallsTotal: 4, Tokens: 500, ElapsedMS: 20},
	}
	got := Summarize(results)
	if len(got) != 3 || got[0].Category != "vocabulary" || got[1].Category != "body" || got[2].Category != "all" {
		t.Fatalf("categories %+v", got)
	}
	v := got[0]
	if v.Questions != 2 || v.Hits != 1 || v.HitRate != 0.5 || v.MeanCallsToHit != 2 || v.MeanCalls != 4 || v.MeanTokens != 500 || v.MedianTokens != 100 || v.P90Tokens != 900 {
		t.Fatalf("vocabulary summary %+v", v)
	}
	all := got[2]
	if all.Questions != 3 || all.Hits != 2 || all.MeanCallsToHit != 3 || all.MedianTokens != 500 || all.P90Tokens != 900 || all.MeanElapsedMS != 20 {
		t.Fatalf("all summary %+v", all)
	}
}

func TestPercentile(t *testing.T) {
	sorted := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if p := percentile(sorted, 0.5); p != 5 {
		t.Fatalf("p50 = %d", p)
	}
	if p := percentile(sorted, 0.9); p != 9 {
		t.Fatalf("p90 = %d", p)
	}
	if p := percentile([]int{7}, 0.9); p != 7 {
		t.Fatalf("single p90 = %d", p)
	}
}

func TestMarkdown(t *testing.T) {
	r := Report{Strategy: "lookup-fetch", Endpoint: "mark://x", Scope: "/", Tokenizer: "o200k_base",
		Questions: []QuestionResult{{ID: "b1", Category: CategoryBody, Query: "q", ExpectedPath: "/a.md", ExpectedAnchor: "s", Hit: true, LookupRank: 2, CallsTotal: 3, Tokens: 42}}}
	r.Summaries = Summarize(r.Questions)
	md := r.Markdown()
	for _, want := range []string{"| body | 1 | 1 | 100% |", "| all | 1 | 1 |", "| b1 | body | q | /a.md#s | hit | 2 | 3 | 42 |"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
}
