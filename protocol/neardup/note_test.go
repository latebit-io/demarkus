package neardup

import "testing"

func TestNote(t *testing.T) {
	hits := []Hit{
		{Path: "/notes/shards.md", Verdict: VerdictNearDuplicate, Score: 0.76},
		{Path: "/patterns.md", Anchor: "shards", Verdict: VerdictContained, Score: 0.93},
	}
	want := "near-duplicate check: near-duplicate of /notes/shards.md (0.76 shared), contained in /patterns.md#shards (0.93); fix: keep one document, append to or link the existing one"
	if got := Note(hits); got != want {
		t.Errorf("Note =\n%q\nwant\n%q", got, want)
	}
	if Note(nil) != "" {
		t.Error("no hits made a note")
	}
	if got := hits[1].Why(); got != "contained (0.93)" {
		t.Errorf("Why = %q", got)
	}
}
