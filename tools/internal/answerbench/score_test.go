package answerbench

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func testFixture(t *testing.T) Fixture {
	t.Helper()
	f, err := LoadFixture()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func validAnswerTrace(t *testing.T, f *Fixture, id string) Trace {
	t.Helper()
	rubric := f.Rubrics[id]
	answer := Answer{Answer: rubric.Answer, Abstain: rubric.Abstain}
	trace := Trace{UsageComplete: true}
	for field, evidence := range rubric.Evidence {
		for _, e := range evidence {
			section, err := f.Section(e)
			if err != nil || !section.Found {
				t.Fatalf("fixture evidence unavailable: %+v: %v", e, err)
			}
			url := sourceKey(e)
			answer.Citations = append(answer.Citations, Citation{Field: field, URL: url, Quote: e.Quote})
			trace.Calls = append(trace.Calls, ToolCall{Name: "fixture_mark_fetch", Input: map[string]any{"url": url}, Output: "status: ok\nversion: " + strconv.Itoa(e.Version) + "\n\n" + section.Text})
		}
	}
	if rubric.Abstain {
		trace.Calls = append(trace.Calls, ToolCall{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/", "query": "pager"}, Output: "status: ok\nmatches: 0\n"})
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	trace.Final = string(raw)
	return trace
}

func TestFrozenRubricAndAllPositiveControls(t *testing.T) {
	f := testFixture(t)
	for _, task := range f.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			trace := validAnswerTrace(t, &f, task.ID)
			if score := scoreTrace(t, &f, task.ID, &trace); !score.Correct {
				t.Fatalf("positive control failed: %+v", score)
			}
		})
	}
}

func TestScorerRejectsUnsupportedOrWrongAnswers(t *testing.T) {
	f := testFixture(t)
	for _, tc := range []struct {
		name   string
		mutate func(*Trace)
	}{
		{"wrong-value", func(trace *Trace) { trace.Final = strings.Replace(trace.Final, `"days":7`, `"days":3`, 1) }},
		{"wrong-version", func(trace *Trace) { trace.Final = strings.ReplaceAll(trace.Final, "/v2#", "/v1#") }},
		{"invented-quote", func(trace *Trace) {
			trace.Final = strings.Replace(trace.Final, "interval is 7 days", "interval is 99 days", 1)
		}},
		{"unread-source", func(trace *Trace) { trace.Calls[0].Output = "status: ok\nversion: 2\n\nOnly a title." }},
		{"wrong-revision-returned", func(trace *Trace) {
			trace.Calls[0].Output = strings.Replace(trace.Calls[0].Output, "version: 2", "version: 1", 1)
		}},
		{"outline-only", func(trace *Trace) {
			trace.Calls[0].Output = "status: ok\nversion: 2\nmode: outline\n\n" + trace.Calls[0].Output
		}},
		{"no-tools", func(trace *Trace) { trace.Calls = nil }},
		{"wrong-host", func(trace *Trace) {
			trace.Final = strings.Replace(trace.Final, `"url":"/`, `"url":"mark://elsewhere/`, 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := validAnswerTrace(t, &f, "q1")
			tc.mutate(&trace)
			if score := scoreTrace(t, &f, "q1", &trace); score.Correct {
				t.Fatal("unsupported answer accepted")
			}
		})
	}
}

func TestMultiDocumentNeedsBothSources(t *testing.T) {
	f := testFixture(t)
	trace := validAnswerTrace(t, &f, "q2")
	trace.Calls = trace.Calls[:1]
	if score := scoreTrace(t, &f, "q2", &trace); score.Correct {
		t.Fatal("single source accepted for a two-source answer")
	}
}

func TestMultiDocumentRequiresDependencyProvenance(t *testing.T) {
	f := testFixture(t)
	trace := validAnswerTrace(t, &f, "q2")
	var answer Answer
	if err := json.Unmarshal([]byte(trace.Final), &answer); err != nil {
		t.Fatal(err)
	}
	kept := answer.Citations[:0]
	for _, citation := range answer.Citations {
		if !strings.Contains(citation.URL, "#audit-handoff") {
			kept = append(kept, citation)
		}
	}
	answer.Citations = kept
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	trace.Final = string(raw)
	if score := scoreTrace(t, &f, "q2", &trace); score.Correct {
		t.Fatal("missing dependency provenance accepted")
	}
}

func TestLookupRequiresExpandedEvidenceNotSnippet(t *testing.T) {
	f := testFixture(t)
	trace := validAnswerTrace(t, &f, "q1")
	quote := f.Rubrics["q1"].Evidence["days"][0].Quote
	trace.Calls = []ToolCall{{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/"}, Output: "| /ops/retention.md#retention | " + quote + " |\n"}}
	if score := scoreTrace(t, &f, "q1", &trace); score.Correct {
		t.Fatal("snippet scored as expanded evidence")
	}
	trace.Calls[0].Output += ">>> /ops/retention.md#retention\n\n" + quote + "\n>>> note: done\n"
	if score := scoreTrace(t, &f, "q1", &trace); !score.Correct {
		t.Fatalf("expanded evidence rejected: %+v", score)
	}
}

func TestReaderInputExcludesTaskCategoryAndAnswerKeys(t *testing.T) {
	prompt, err := taskPrompt(Task{ID: "PRIVATE-ID", Category: "PRIVATE-CATEGORY", Question: "How long?", Fields: map[string]string{"days": "number"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "PRIVATE") || prompt != "How long?\nAnswer fields and types: {\"days\":\"number\"}" {
		t.Fatalf("unexpected reader input: %s", prompt)
	}
}

func TestSupplementalSupportDoesNotReplaceRequiredEvidence(t *testing.T) {
	f := testFixture(t)
	const extra = "The seven-day interval is the replacement policy."
	for i := range f.Documents {
		if f.Documents[i].Path == "/ops/retention.md" {
			f.Documents[i].Versions[1] += "\n" + extra + "\n"
		}
	}
	trace := validAnswerTrace(t, &f, "q1")
	var answer Answer
	if err := json.Unmarshal([]byte(trace.Final), &answer); err != nil {
		t.Fatal(err)
	}
	answer.Citations = append(answer.Citations, Citation{Field: "days", URL: "/ops/retention.md/v2#retention", Quote: extra})
	setAnswer := func() {
		raw, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		trace.Final = string(raw)
	}
	setAnswer()
	if scoreTrace(t, &f, "q1", &trace).Correct {
		t.Fatal("unapproved extra quote accepted")
	}
	rubric := f.Rubrics["q1"]
	rubric.Supplemental = map[string][]Evidence{"days": {{Path: "/ops/retention.md", Version: 2, Anchor: "retention", Quote: extra}}}
	f.Rubrics["q1"] = rubric
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	if score := scoreTrace(t, &f, "q1", &trace); !score.Correct {
		t.Fatalf("approved supporting quote rejected: %+v", score)
	}
	answer.Citations = answer.Citations[1:]
	setAnswer()
	if scoreTrace(t, &f, "q1", &trace).Correct {
		t.Fatal("supplemental quote replaced required evidence")
	}
}

func scoreTrace(t *testing.T, f *Fixture, task string, trace *Trace) Score {
	t.Helper()
	score, err := f.Score(task, trace, "127.0.0.1:16319")
	if err != nil {
		t.Fatal(err)
	}
	return score
}
