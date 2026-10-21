package neardup

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
)

// targets is a scan over analyzed bodies keyed by path, every path but the
// draft's own.
func targets(docs map[string]string, own string) func(yield func(Target) bool) {
	return func(yield func(Target) bool) {
		for _, p := range slices.Sorted(maps.Keys(docs)) {
			if p == own {
				continue
			}
			d := Analyze(docs[p])
			if !yield(d.Target(p)) {
				return
			}
		}
	}
}

func TestScan(t *testing.T) {
	docs := map[string]string{
		"/lesson.md":   neardtest.LessonDoc,
		"/reworded.md": neardtest.RewordedDoc,
		"/hub.md":      neardtest.HubDoc,
		"/other.md":    neardtest.OtherDoc,
	}
	t.Run("a reworded copy is a near-duplicate, a hub section holds it", func(t *testing.T) {
		got := Scan(Analyze(neardtest.LessonDoc).Whole, targets(docs, "/lesson.md"))
		if len(got) != 2 {
			t.Fatalf("hits = %+v, want the reworded copy and the hub", got)
		}
		if got[0].Path != "/reworded.md" || got[0].Verdict != VerdictNearDuplicate || got[0].Score < NearDuplicate {
			t.Errorf("first hit = %+v, want near-duplicate of /reworded.md", got[0])
		}
		if got[1].Path != "/hub.md" || got[1].Anchor != "shards" || got[1].Verdict != VerdictContained || got[1].Score < Contained {
			t.Errorf("second hit = %+v, want contained in /hub.md#shards", got[1])
		}
	})
	t.Run("the whole hub is not a candidate, its section is", func(t *testing.T) {
		draft, hub := Analyze(neardtest.LessonDoc).Whole, Analyze(neardtest.HubDoc)
		if Hamming(draft.Sig, hub.Whole.Sig) <= MaxDistance {
			t.Error("whole hub within MaxDistance of one section; the section index is pointless")
		}
		if len(hub.Sections) != 3 || hub.Sections[0].Sig != 0 || hub.Sections[1].Sig == 0 {
			t.Errorf("hub sections = %+v, want an unsigned intro and signed shards", hub.Sections)
		}
	})
	t.Run("a single-section body has no sections", func(t *testing.T) {
		d := Analyze(neardtest.OtherDoc)
		if len(d.Sections) != 1 || d.Whole.Sig == 0 || d.Sections[0].Sig != d.Whole.Sig {
			t.Errorf("Analyze(single) = %+v, want one section signed as the whole", d)
		}
		if w := AnalyzeWhole(neardtest.OtherDoc); w.Sig != d.Whole.Sig || len(w.Set) != len(d.Whole.Set) {
			t.Errorf("AnalyzeWhole = %+v, want the whole of Analyze", w)
		}
	})
	t.Run("shared scaffolding is not a duplicate", func(t *testing.T) {
		scaffold := "Status: accepted. Part of the decisions hub. Context, decision, consequences and alternatives considered follow.\n\n"
		adrs := map[string]string{"/a.md": "# A\n\n" + scaffold + neardtest.Lesson + "\n", "/b.md": "# B\n\n" + scaffold + neardtest.Other + "\n"}
		if got := Scan(Analyze(adrs["/a.md"]).Whole, targets(adrs, "/a.md")); got != nil {
			t.Errorf("hits = %+v, want none", got)
		}
	})
	t.Run("a short draft has no signature and no hits", func(t *testing.T) {
		note := "# Note\n\n" + strings.Join(strings.Fields(neardtest.Lesson)[:30], " ") + "\n"
		if d := Analyze(note); d.Whole.Sig != 0 || d.Whole.Set != nil {
			t.Errorf("30-token note signed: %+v", d.Whole)
		}
		if got := Scan(Analyze(note).Whole, targets(docs, "")); got != nil {
			t.Errorf("hits = %+v, want none", got)
		}
	})
}

func TestJudgeAndSets(t *testing.T) {
	a := SetOf(slices.Values([]string{"alpha", "beta", "gamma", "of"}))
	b := SetOf(slices.Values([]string{"alpha", "beta", "delta"}))
	if len(a) != 3 {
		t.Errorf("short token kept: %d hashes", len(a))
	}
	if got := a.Jaccard(b); got != 0.5 {
		t.Errorf("Jaccard = %v, want 0.5", got)
	}
	if got := (Set{}).Jaccard(Set{}); got != 0 {
		t.Errorf("empty Jaccard = %v", got)
	}
	// Full containment in a shorter target is no verdict; in a section it is.
	if _, ok := judge(a, a[:2], false); ok {
		t.Error("a shorter whole target contained the draft")
	}
	if h, ok := judge(a[:2], a, true); !ok || h.Verdict != VerdictContained || h.Score != 1 {
		t.Errorf("section holding the draft = %+v, %v", h, ok)
	}
}
