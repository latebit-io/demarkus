package answerbench

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func scopedAnswerFixture(t *testing.T, id string) (Fixture, Task, Trace) {
	t.Helper()
	f := testFixture(t)
	var task Task
	for _, candidate := range f.Tasks {
		if candidate.ID == id {
			task = candidate
			break
		}
	}
	task.Scope = "/"
	f.Tasks = []Task{task}
	rubric := f.Rubrics[id]
	rubric.Outcome = "answered"
	complete := true
	rubric.Completion = []Completion{{Step: "scope", Tool: "mark_lookup", URL: "/", Query: "complete relevant scope", Match: "body", Status: "ok", Complete: &complete}}
	f.Rubrics = map[string]Rubric{id: rubric}
	trace := validAnswerTrace(t, &f, id)
	trace.Calls = append([]ToolCall{{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/", "query": "complete relevant scope", "match": "body"}, Output: "status: ok\nmatch: body\ncomplete: true\n"}}, trace.Calls...)
	var answer Answer
	if err := json.Unmarshal([]byte(trace.Final), &answer); err != nil {
		t.Fatal(err)
	}
	answer.Outcome = "answered"
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	trace.Final = string(raw)
	return f, task, trace
}

func TestIndependentScoreSeparatesAnswerEvidenceAndCitations(t *testing.T) {
	f, _, trace := scopedAnswerFixture(t, "q2")
	score, err := f.Score("q2", &trace, "127.0.0.1:16319")
	if err != nil || !score.Correct || !score.AnswerCorrect || !score.EvidenceSufficient || !score.CitationsValid || !score.ScopeComplete {
		t.Fatalf("positive score=%+v, err=%v", score, err)
	}
	var answer Answer
	if err := json.Unmarshal([]byte(trace.Final), &answer); err != nil {
		t.Fatal(err)
	}
	answer.Citations = answer.Citations[:len(answer.Citations)-1]
	trace.Calls = trace.Calls[:len(trace.Calls)-1]
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	trace.Final = string(raw)
	score, err = f.Score("q2", &trace, "127.0.0.1:16319")
	if err != nil {
		t.Fatal(err)
	}
	if score.Correct || !score.AnswerCorrect || score.EvidenceSufficient || !score.CitationsValid || !score.ScopeComplete {
		t.Fatalf("missing dependency dimensions=%+v", score)
	}
	_, _, scopeTrace := scopedAnswerFixture(t, "q2")
	scopeTrace.Calls = scopeTrace.Calls[1:]
	score, err = f.Score("q2", &scopeTrace, "127.0.0.1:16319")
	if err != nil || !score.Correct || !score.AnswerCorrect || !score.EvidenceSufficient || !score.CitationsValid || score.ScopeComplete {
		t.Fatalf("missing scope completion dimensions=%+v err=%v", score, err)
	}
	f.Dataset = &DatasetManifest{ScoringVersion: independentScoringVersionV1}
	score, err = f.Score("q2", &scopeTrace, "127.0.0.1:16319")
	if err != nil || score.Correct || score.ScopeComplete {
		t.Fatalf("v1 accepted missing scope completion: score=%+v err=%v", score, err)
	}
}

func TestIndependentScoreKeepsEvidenceDimensionWithExtraInvalidCitation(t *testing.T) {
	f, _, trace := scopedAnswerFixture(t, "q2")
	var answer Answer
	if err := json.Unmarshal([]byte(trace.Final), &answer); err != nil {
		t.Fatal(err)
	}
	answer.Citations = append(answer.Citations, Citation{Field: "days", URL: "outside"})
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	trace.Final = string(raw)
	score, err := f.Score("q2", &trace, "127.0.0.1:16319")
	if err != nil || score.Correct || !score.AnswerCorrect || !score.EvidenceSufficient || score.CitationsValid || !score.ScopeComplete {
		t.Fatalf("invalid extra citation collapsed dimensions: score=%+v err=%v", score, err)
	}
}

func TestIndependentAbsenceRequiresCompleteSuccessfulScope(t *testing.T) {
	f := testFixture(t)
	task := Task{ID: "absence", Category: "absent", Question: "What is the pager?", Fields: map[string]string{"pager": "string"}, Scope: "/"}
	zero := 0
	f.Tasks = []Task{task}
	f.Rubrics = map[string]Rubric{"absence": {
		Outcome:    "not-found",
		Answer:     map[string]json.RawMessage{},
		Evidence:   map[string][]Evidence{},
		Completion: []Completion{{Step: "search", Tool: "mark_lookup", URL: "/", Query: "emergency pager telephone", Match: "body", Status: "ok", Matches: &zero}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	answer := `{"answer":{},"citations":[],"abstain":true,"outcome":"not-found"}`
	trace := Trace{Final: answer, Calls: []ToolCall{{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/", "query": "emergency pager telephone", "match": "body"}, Output: "status: ok\nmatches: 0\nmatch: body\n"}}}
	score, err := f.Score("absence", &trace, "127.0.0.1:16319")
	if err != nil || !score.Correct || !score.ScopeComplete {
		t.Fatalf("complete absence score=%+v, err=%v", score, err)
	}
	rubric := f.Rubrics["absence"]
	second := rubric.Completion[0]
	second.Step = "confirm"
	rubric.Completion = append(rubric.Completion, second)
	f.Rubrics["absence"] = rubric
	score, err = f.Score("absence", &trace, "127.0.0.1:16319")
	if err != nil || score.Correct || score.ScopeComplete {
		t.Fatalf("one call satisfied multiple completion steps: score=%+v err=%v", score, err)
	}
	trace.Calls = append(trace.Calls, trace.Calls[0])
	score, err = f.Score("absence", &trace, "127.0.0.1:16319")
	if err != nil || !score.Correct || !score.ScopeComplete {
		t.Fatalf("distinct completion calls rejected: score=%+v err=%v", score, err)
	}
	trace.Calls = trace.Calls[:1]
	rubric.Completion = rubric.Completion[:1]
	f.Rubrics["absence"] = rubric
	trace.Calls[0].Error = "lookup failed"
	score, err = f.Score("absence", &trace, "127.0.0.1:16319")
	if err != nil {
		t.Fatal(err)
	}
	if score.Correct || score.ScopeComplete || score.EvidenceSufficient {
		t.Fatalf("failed scope established absence: %+v", score)
	}
}

func TestIndependentIncompleteOutcomeNeedsVisibleFailure(t *testing.T) {
	f := testFixture(t)
	task := Task{ID: "incomplete", Category: "incomplete-scope", Question: "What is outside this scope?", Fields: map[string]string{"value": "string"}, Scope: "/limited"}
	f.Tasks = []Task{task}
	f.Rubrics = map[string]Rubric{"incomplete": {
		Outcome: "incomplete",
		Answer:  map[string]json.RawMessage{}, Evidence: map[string][]Evidence{},
		Completion: []Completion{{Step: "search", Tool: "mark_lookup", URL: "/limited", Query: "outside fact", Match: "body", Failure: true}},
	}}
	trace := Trace{Final: `{"answer":{},"citations":[],"abstain":true,"outcome":"incomplete"}`, Calls: []ToolCall{{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/limited", "query": "outside fact", "match": "body"}, Error: "partial scope"}}}
	score, err := f.Score("incomplete", &trace, "127.0.0.1:16319")
	if err != nil || !score.Correct || score.ScopeComplete || !score.EvidenceSufficient {
		t.Fatalf("incomplete score=%+v, err=%v", score, err)
	}
	trace.Calls[0].Error = ""
	score, err = f.Score("incomplete", &trace, "127.0.0.1:16319")
	if err != nil || score.Correct || score.EvidenceSufficient {
		t.Fatalf("malformed empty result established failure: score=%+v err=%v", score, err)
	}
}

func TestIndependentCompletionMatchingFindsCompleteAssignment(t *testing.T) {
	f := testFixture(t)
	task := Task{ID: "overlap", Category: "incomplete-scope", Question: "What is outside this scope?", Fields: map[string]string{"value": "string"}, Scope: "/limited"}
	f.Tasks = []Task{task}
	f.Rubrics = map[string]Rubric{"overlap": {
		Outcome: "incomplete", Answer: map[string]json.RawMessage{}, Evidence: map[string][]Evidence{},
		Completion: []Completion{
			{Step: "broad", Tool: "mark_lookup", URL: "/limited", Query: "outside fact", Failure: true},
			{Step: "body", Tool: "mark_lookup", URL: "/limited", Query: "outside fact", Match: "body", Failure: true},
		},
	}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	trace := Trace{
		Final: `{"answer":{},"citations":[],"abstain":true,"outcome":"incomplete"}`,
		Calls: []ToolCall{
			{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/limited", "query": "outside fact", "match": "body"}, Error: "partial body scope"},
			{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/limited", "query": "outside fact", "match": "catalog"}, Error: "partial catalog scope"},
		},
	}
	score, err := f.Score("overlap", &trace, "127.0.0.1:16319")
	if err != nil || !score.Correct || !score.EvidenceSufficient {
		t.Fatalf("complete matching rejected valid assignment: score=%+v err=%v", score, err)
	}
	rubric := f.Rubrics["overlap"]
	rubric.Completion[0].Query = ""
	f.Rubrics["overlap"] = rubric
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "invalid completion key") {
		t.Fatalf("empty match and query accepted: %v", err)
	}
}

func TestIndependentIncompleteOutcomeAcceptsExplicitPartialCompletion(t *testing.T) {
	f := testFixture(t)
	task := Task{ID: "partial", Category: "incomplete-scope", Question: "What is outside this scope?", Fields: map[string]string{"value": "string"}, Scope: "/limited"}
	complete := false
	f.Tasks = []Task{task}
	f.Rubrics = map[string]Rubric{"partial": {
		Outcome: "incomplete", Answer: map[string]json.RawMessage{}, Evidence: map[string][]Evidence{},
		Completion: []Completion{{Step: "search", Tool: "mark_lookup", URL: "/limited", Query: "outside fact", Match: "body", Status: "ok", Complete: &complete}},
	}}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	trace := Trace{Final: `{"answer":{},"citations":[],"abstain":true,"outcome":"incomplete"}`, Calls: []ToolCall{{Name: "fixture_mark_lookup", Input: map[string]any{"url": "/limited", "query": "outside fact", "match": "body"}, Output: "status: ok\nmatch: body\ncomplete: false\n"}}}
	score, err := f.Score("partial", &trace, "127.0.0.1:16319")
	if err != nil || !score.Correct || score.ScopeComplete || !score.EvidenceSufficient {
		t.Fatalf("partial completion score=%+v, err=%v", score, err)
	}
}

func TestIndependentScoringIsDeterministicAndRetainsContradictions(t *testing.T) {
	f, _, trace := scopedAnswerFixture(t, "q5")
	rubric := f.Rubrics["q5"]
	rubric.Contradictions = []Evidence{rubric.Evidence["east"][0], rubric.Evidence["west"][0]}
	f.Rubrics["q5"] = rubric
	first, err := f.Score("q5", &trace, "127.0.0.1:16319")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Score("q5", &trace, "127.0.0.1:16319")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || len(first.ContradictionsObserved) != 2 {
		t.Fatalf("non-deterministic or hidden contradictions: first=%+v second=%+v", first, second)
	}
}
