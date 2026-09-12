package retrievalbench

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/lookupexpand"
	"github.com/latebit-io/demarkus/client/lookuptable"
)

//go:embed questions/graph-context-v1.json
var graphContextFixture []byte

type baselineEvidence struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

type baselineContextCase struct {
	Name     string             `json:"name"`
	Query    string             `json:"query"`
	Budget   int                `json:"budget_bytes"`
	Rows     []string           `json:"rows"`
	Docs     map[string]string  `json:"docs"`
	Expected []baselineEvidence `json:"expected"`
}

type baselineEvidenceScore struct {
	Found      int
	Duplicates int
}

// Only framed source text is evidence. Tables, snippets, and notes cannot score.
func scoreBaselineEvidence(expansion string, expected []baselineEvidence) baselineEvidenceScore {
	counts := make([]int, len(expected))
	var path string
	var body strings.Builder
	scoreBlock := func() {
		if path == "" {
			return
		}
		text := body.String()
		for i, evidence := range expected {
			if path == evidence.Path {
				counts[i] += strings.Count(text, evidence.Text)
			}
		}
	}
	for line := range strings.SplitSeq(expansion, "\n") {
		location, frame := strings.CutPrefix(line, lookupexpand.Delimiter+" ")
		if !frame {
			body.WriteString(line)
			body.WriteByte('\n')
			continue
		}
		scoreBlock()
		body.Reset()
		path = ""
		if !strings.HasPrefix(location, "note: ") {
			path, _ = lookuptable.SplitLocation(location)
		}
	}
	scoreBlock()
	var score baselineEvidenceScore
	for _, count := range counts {
		if count > 0 {
			score.Found++
			score.Duplicates += count - 1
		}
	}
	return score
}

func loadBaselineContext(t testing.TB) []baselineContextCase {
	t.Helper()
	var cases []baselineContextCase
	if err := json.Unmarshal(graphContextFixture, &cases); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, tc := range cases {
		if tc.Name == "" || names[tc.Name] || tc.Budget <= 0 || len(tc.Rows) == 0 || len(tc.Expected) == 0 {
			t.Fatalf("invalid context fixture: %s", tc.Name)
		}
		names[tc.Name] = true
		for _, evidence := range tc.Expected {
			if evidence.Text == "" || strings.Count(tc.Docs[evidence.Path], evidence.Text) != 1 {
				t.Fatalf("%s: evidence must occur exactly once in source %s", tc.Name, evidence.Path)
			}
		}
	}
	if len(cases) == 0 {
		t.Fatal("empty context fixture")
	}
	return cases
}

func BenchmarkGraphBaselineContext(b *testing.B) {
	counter, err := NewO200kCounter()
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range loadBaselineContext(b) {
		b.Run(tc.Name, func(b *testing.B) {
			var table strings.Builder
			table.WriteString("| Path | Importance | Title | Tags |\n|---|---|---|---|\n")
			for _, row := range tc.Rows {
				fmt.Fprintf(&table, "| %s | 0.8 | Fixture | graph |\n", row)
			}
			rows := table.String()
			var fetches int
			fetchDoc := func(_ context.Context, path string) (string, error) {
				fetches++
				body, ok := tc.Docs[path]
				if !ok {
					return "", fmt.Errorf("fixture source %s: not-found", path)
				}
				return body, nil
			}
			var expansion string
			for b.Loop() {
				expansion = lookupexpand.Expand(b.Context(), rows, tc.Query, tc.Budget, fetchDoc)
			}
			// Scoring and tokenization never influence retrieval or its timer.
			score := scoreBaselineEvidence(expansion, tc.Expected)
			response := rows + expansion
			tokens, err := counter.Count(response)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(score.Found)/float64(len(tc.Expected)), "evidence-recall")
			b.ReportMetric(float64(score.Duplicates), "duplicate-evidence/op")
			b.ReportMetric(float64(tokens), "result-tokens/op")
			b.ReportMetric(float64(max(0, len(response)-tc.Budget)), "over-budget-B/op")
			b.ReportMetric(float64(fetches)/float64(b.N), "fetches/op")
		})
	}
}

func TestBaselineEvidenceScorer(t *testing.T) {
	evidence := []baselineEvidence{{Path: "/a.md", Text: "Keep seven versions."}, {Path: "/b.md", Text: "Retain the audit trail."}}
	for _, tc := range []struct {
		name, expansion string
		want            baselineEvidenceScore
	}{
		{"snippet", "| /a.md#policy | Keep seven versions. |", baselineEvidenceScore{}},
		{"wrong-source", ">>> /wrong.md#policy\n\nKeep seven versions.", baselineEvidenceScore{}},
		{"note", ">>> note: /a.md Keep seven versions.", baselineEvidenceScore{}},
		{"path-only", ">>> /a.md#policy\n\n", baselineEvidenceScore{}},
		{"partial", ">>> /a.md#policy\n\nKeep seven versions.", baselineEvidenceScore{Found: 1}},
		{"complete", ">>> /a.md#policy\n\nKeep seven versions.\n>>> /b.md#audit\n\nRetain the audit trail.", baselineEvidenceScore{Found: 2}},
		{"overlap", ">>> /a.md#policy\n\nKeep seven versions.\n>>> /a.md#child\n\nKeep seven versions.", baselineEvidenceScore{Found: 1, Duplicates: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreBaselineEvidence(tc.expansion, evidence); got != tc.want {
				t.Fatalf("score=%+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBaselineContextFixture(t *testing.T) {
	loadBaselineContext(t)
}
