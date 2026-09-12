package answerbench

import (
	"fmt"
	"os"
)

// Rescore keeps original traces and spend, recording the prior report's digest.
// It cannot regrade changed sources/tasks or overwrite an existing report.
func Rescore(beforePath, outputPath string) (report Report, err error) {
	f, err := LoadFixture()
	if err != nil {
		return report, err
	}
	return RescoreWithFixture(beforePath, outputPath, &f)
}

// RescoreWithFixture supports copied corpora with the same provenance checks.
func RescoreWithFixture(beforePath, outputPath string, f *Fixture) (report Report, err error) {
	raw, err := os.ReadFile(beforePath)
	if err != nil {
		return report, err
	}
	if err := decodeJSON(raw, &report); err != nil {
		return report, err
	}
	if report.MetricsOnly {
		return report, fmt.Errorf("metrics-only reports cannot be rescored; use the private original trace")
	}
	if report.Spec.Hashes["corpus"] != f.Hashes["corpus"] || report.Spec.Hashes["tasks"] != f.Hashes["tasks"] {
		return report, fmt.Errorf("rescore requires the original corpus and tasks")
	}
	complete := report.Summary.UsageComplete
	cfg := Config{Origin: report.Spec.Origin, Port: report.Spec.Port}
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		attempt.Score, err = f.Score(attempt.Task, &attempt.Trace, cfg.logicalHost())
		if err != nil {
			return report, err
		}
	}
	report.Spec.Hashes["rubric"] = f.Hashes["rubric"]
	report.Spec.ScoringVersion = scoringVersion
	report.RescoredFrom = digest(raw)
	report.summarize()
	if !complete || len(report.Attempts) != report.Spec.ExpectedAttempts {
		report.Summary.UsageComplete = false
		report.Summary.TokensPerCorrect = nil
	}
	err = writeNewJSON(outputPath, &report)
	return report, err
}
