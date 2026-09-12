package answerbench

import "testing"

func comparisonReport() Report {
	r := Report{Spec: RunSpec{Suite: "test", Model: "fixed/model", Repeats: 1, ExpectedAttempts: 2}}
	r.Attempts = []Attempt{
		{Task: "q1", Repeat: 1, Trace: Trace{Usage: Usage{Input: 100}, UsageComplete: true}, Score: Score{Correct: true}},
		{Task: "q2", Repeat: 1, Trace: Trace{Usage: Usage{Input: 100}, UsageComplete: true}, Score: Score{Correct: true}},
	}
	r.Summary = Summarize(r.Attempts)
	return r
}

func TestComparisonProtectsAccuracyAndComparability(t *testing.T) {
	for _, tc := range []struct {
		name                string
		mutate              func(*Report)
		wantError, improved bool
	}{
		{"lower-cost", func(r *Report) { r.Attempts[0].Trace.Usage.Input = 50 }, false, true},
		{"lost-answer", func(r *Report) { r.Attempts[0].Trace.Usage.Input = 1; r.Attempts[1].Score.Correct = false }, false, false},
		{"different-model", func(r *Report) { r.Spec.Model = "another/model" }, true, false},
		{"different-fixture", func(r *Report) { r.Spec.Hashes = map[string]string{"corpus": "changed"} }, true, false},
		{"incomplete-usage", func(r *Report) { r.Attempts[1].Trace.UsageComplete = false }, true, false},
		{"partial-cohort", func(r *Report) { r.Attempts = r.Attempts[:1] }, true, false},
		{"duplicate-attempt", func(r *Report) { r.Attempts[1].Task = "q1" }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := comparisonReport(), comparisonReport()
			tc.mutate(&after)
			comparison, err := Compare(&before, &after)
			if (err != nil) != tc.wantError || comparison.ImprovedPointEstimate != tc.improved {
				t.Fatalf("comparison=%+v err=%v", comparison, err)
			}
		})
	}
}
