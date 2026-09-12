package answerbench

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

func eventLine(t *testing.T, kind, message, part string, extra map[string]any) string {
	t.Helper()
	fields := map[string]any{"id": part, "messageID": message}
	maps.Copy(fields, extra)
	raw, err := json.Marshal(map[string]any{"type": kind, "sessionID": "s1", "part": fields})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func usageEvents(t *testing.T) string {
	t.Helper()
	return eventLine(t, "step_start", "m1", "p1", nil) +
		eventLine(t, "step_finish", "m1", "p2", map[string]any{"reason": "tool-calls", "tokens": map[string]any{"input": 100, "output": 10, "reasoning": 20, "cache": map[string]int{"read": 50, "write": 5}, "total": 185}}) +
		eventLine(t, "step_start", "m2", "p3", nil) +
		eventLine(t, "text", "m2", "p4", map[string]any{"text": "final answer"}) +
		eventLine(t, "step_finish", "m2", "p5", map[string]any{"reason": "stop", "tokens": map[string]any{"input": 200, "output": 4, "reasoning": 1, "cache": map[string]int{"read": 100, "write": 0}, "total": 305}})
}

func TestProviderUsageIncludesCacheReasoningAndEarlierTurns(t *testing.T) {
	raw := usageEvents(t)
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	raw += lines[1] + "\n" // Redelivered part must not charge the same usage twice.
	trace, err := ParseEvents(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !trace.UsageComplete || trace.Turns != 2 || trace.Usage.Tokens() != 490 || trace.Final != "final answer" {
		t.Fatalf("incorrect usage aggregation: %+v", trace)
	}
}

func TestProviderUsageRejectsIncompleteOrInconsistentEvents(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantError bool
	}{
		{"unfinished-turn", usageEvents(t) + eventLine(t, "step_start", "m3", "p6", nil), false},
		{"no-usage", eventLine(t, "step_start", "m1", "p1", nil) + eventLine(t, "step_finish", "m1", "p2", map[string]any{"reason": "stop"}), true},
		{"double-counted-total", strings.Replace(usageEvents(t), `"total":185`, `"total":370`, 1), true},
		{"negative-bucket", strings.Replace(usageEvents(t), `"input":100`, `"input":-100`, 1), true},
		{"mixed-session", usageEvents(t) + strings.Replace(eventLine(t, "text", "m3", "p6", map[string]any{"text": "other"}), `"s1"`, `"s2"`, 1), true},
		{"malformed", usageEvents(t) + "not-json\n", true},
		{"provider-error", usageEvents(t) + "{\"type\":\"error\",\"sessionID\":\"s1\",\"error\":{\"message\":\"rate limited\"}}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace, err := ParseEvents(strings.NewReader(tc.raw))
			if (err != nil) != tc.wantError || trace.UsageComplete {
				t.Fatalf("complete=%t err=%v", trace.UsageComplete, err)
			}
		})
	}
}

func TestFailedAnswersRemainInTokenNumerator(t *testing.T) {
	attempts := []Attempt{
		{Trace: Trace{Usage: Usage{Input: 100}, UsageComplete: true}, Score: Score{Correct: true}},
		{Trace: Trace{Usage: Usage{Input: 300}, UsageComplete: true}, Score: Score{Correct: false}},
	}
	summary := Summarize(attempts)
	if summary.TokensPerCorrect == nil || *summary.TokensPerCorrect != 400 {
		t.Fatalf("failed answer spend omitted: %+v", summary)
	}
	attempts[0].Score.Correct = false
	if summary := Summarize(attempts); summary.TokensPerCorrect != nil {
		t.Fatalf("zero correct answers must not produce a finite ratio: %+v", summary)
	}
	attempts[0].Score.Correct = true
	attempts[1].Trace.UsageComplete = false
	if summary := Summarize(attempts); summary.TokensPerCorrect != nil || summary.KnownTokens != 400 {
		t.Fatalf("unknown spend must invalidate ratio, retaining known spend: %+v", summary)
	}
}
