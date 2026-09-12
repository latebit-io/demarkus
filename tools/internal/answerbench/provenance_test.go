package answerbench

import (
	"encoding/json"
	"testing"
)

func TestEvidenceCannotCrossSiblingSections(t *testing.T) {
	f := testFixture(t)
	quote := f.Rubrics["q1"].Evidence["days"][0].Quote
	for i := range f.Documents {
		if f.Documents[i].Path == "/ops/retention.md" {
			f.Documents[i].Versions[1] = "# Policy\n\n## Retention\n\n" + quote + "\n\n## Other\n\n" + quote + "\n"
		}
	}
	for _, tc := range []struct {
		name, fetchedAnchor, citedAnchor string
		lookup, headerScope              bool
		correct                          bool
	}{
		{"correct-section", "retention", "retention", false, false, true},
		{"wrong-fetch-scope", "other", "retention", false, false, false},
		{"wrong-citation-scope", "retention", "other", false, false, false},
		{"wrong-citation-after-whole-fetch", "", "other", false, false, false},
		{"ambiguous-parent-citation", "", "policy", false, false, false},
		{"whole-fetch", "", "retention", false, false, true},
		{"scoped-response-to-bare-fetch", "other", "retention", false, true, false},
		{"lookup-sibling", "other", "retention", true, false, false},
		{"lookup-whole", "", "retention", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := validAnswerTrace(t, &f, "q1")
			var answer Answer
			if err := json.Unmarshal([]byte(trace.Final), &answer); err != nil {
				t.Fatal(err)
			}
			answer.Citations[0].URL = "/ops/retention.md/v2#" + tc.citedAnchor
			raw, err := json.Marshal(answer)
			if err != nil {
				t.Fatal(err)
			}
			trace.Final = string(raw)
			e := Evidence{Path: "/ops/retention.md", Version: 2, Anchor: tc.fetchedAnchor}
			section, err := f.Section(e)
			if err != nil || !section.Found {
				t.Fatalf("section: %+v, %v", section, err)
			}
			url := sourceKey(e)
			output := "status: ok\nversion: 2\n"
			if tc.headerScope {
				url = "/ops/retention.md"
				output += "section: #" + tc.fetchedAnchor + "\n"
			}
			trace.Calls = []ToolCall{{Name: "fixture_mark_fetch", Input: map[string]any{"url": url}, Output: output + "\n" + section.Text}}
			if tc.lookup {
				loc := e.Path
				if e.Anchor != "" {
					loc += "#" + e.Anchor
				}
				trace.Calls[0] = ToolCall{Name: "fixture_mark_lookup", Output: ">>> " + loc + "\n\n" + section.Text}
			}
			if score := scoreTrace(t, &f, "q1", &trace); score.Correct != tc.correct {
				t.Fatalf("score=%+v, want correct=%t", score, tc.correct)
			}
		})
	}
}

func TestObservedParentPreservesNestedAnchor(t *testing.T) {
	f := testFixture(t)
	quote := f.Rubrics["q1"].Evidence["days"][0].Quote
	for i := range f.Documents {
		if f.Documents[i].Path == "/ops/retention.md" {
			f.Documents[i].Versions[1] = "# Policy\n\n## Parent\n\n### Retention\n\n" + quote + "\n"
		}
	}
	trace := validAnswerTrace(t, &f, "q1")
	parent, err := f.Section(Evidence{Path: "/ops/retention.md", Version: 2, Anchor: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	trace.Calls = []ToolCall{{Name: "fixture_mark_fetch", Input: map[string]any{"url": "/ops/retention.md#parent"}, Output: "status: ok\nversion: 2\nsection: #parent\n\n" + parent.Text}}
	if score := scoreTrace(t, &f, "q1", &trace); !score.Correct {
		t.Fatalf("contained source section was lost: %+v", score)
	}
	var answer Answer
	if err := json.Unmarshal([]byte(trace.Final), &answer); err != nil {
		t.Fatal(err)
	}
	answer.Citations[0].URL = "/ops/retention.md/v2#parent"
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	trace.Final = string(raw)
	if score := scoreTrace(t, &f, "q1", &trace); !score.Correct {
		t.Fatalf("unambiguous parent citation was lost: %+v", score)
	}
}

func TestEmbeddedCurrentRevisionValidation(t *testing.T) {
	for _, current := range []int{-1, 0, 1, 2, 3} {
		f := testFixture(t)
		for i := range f.Documents {
			if f.Documents[i].Path == "/ops/retention.md" {
				f.Documents[i].Current = current
			}
		}
		if err := f.Validate(); (err != nil) != (current < 0 || current > 2) {
			t.Errorf("current=%d error=%v", current, err)
		}
	}
}
