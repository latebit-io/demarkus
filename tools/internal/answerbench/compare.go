package answerbench

import (
	"fmt"
	"os"
	"reflect"
	"slices"
)

// Comparison reports cost changes without accepting task-level accuracy regressions.
type Comparison struct {
	Before                Summary  `json:"before"`
	After                 Summary  `json:"after"`
	TokenChangePct        *float64 `json:"tokens_per_correct_change_percent"`
	RegressedTasks        []string `json:"regressed_tasks"`
	ImprovedPointEstimate bool     `json:"improved_point_estimate"`
	Note                  string   `json:"note"`
	BeforePolicy          string   `json:"before_policy,omitempty"`
	AfterPolicy           string   `json:"after_policy,omitempty"`
	BeforeResultTokens    *int     `json:"before_tool_result_tokens,omitempty"`
	AfterResultTokens     *int     `json:"after_tool_result_tokens,omitempty"`
}

// LoadReport rejects report fields from an unrecognized schema.
func LoadReport(path string) (Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var report Report
	if err := decodeJSON(raw, &report); err != nil {
		return report, err
	}
	return report, nil
}

// Compare rejects mismatched experiments and incomplete spend accounting.
func Compare(before, after *Report) (Comparison, error) {
	beforeSpec, afterSpec := before.Spec, after.Spec
	beforeSpec.ReaderPolicy = policyName(beforeSpec.ReaderPolicy)
	afterSpec.ReaderPolicy = policyName(afterSpec.ReaderPolicy)
	if !reflect.DeepEqual(beforeSpec, afterSpec) {
		return Comparison{}, fmt.Errorf("incompatible model, settings, prompt, or fixture; do not compare these runs")
	}
	if before.Spec.ExpectedAttempts <= 0 || len(before.Attempts) != before.Spec.ExpectedAttempts || len(after.Attempts) != after.Spec.ExpectedAttempts {
		return Comparison{}, fmt.Errorf("partial cohort; expected all scheduled attempts")
	}
	beforeCounts, err := taskCounts(before)
	if err != nil {
		return Comparison{}, err
	}
	afterCounts, err := taskCounts(after)
	if err != nil {
		return Comparison{}, err
	}
	c := Comparison{Before: Summarize(before.Attempts), After: Summarize(after.Attempts), Note: "Point estimates only; repeat paired runs before claiming a reliable improvement."}
	if !c.Before.UsageComplete || !c.After.UsageComplete || !before.Summary.UsageComplete || !after.Summary.UsageComplete {
		return c, fmt.Errorf("incomplete token accounting; ratio would hide unknown spend")
	}
	for task, count := range beforeCounts {
		afterCount, exists := afterCounts[task]
		if !exists {
			return c, fmt.Errorf("different task cohorts")
		}
		if afterCount < count {
			c.RegressedTasks = append(c.RegressedTasks, task)
		}
	}
	if len(beforeCounts) != len(afterCounts) {
		return c, fmt.Errorf("different task cohorts")
	}
	slices.Sort(c.RegressedTasks)
	if c.Before.TokensPerCorrect != nil && c.After.TokensPerCorrect != nil && *c.Before.TokensPerCorrect > 0 {
		change := (*c.After.TokensPerCorrect / *c.Before.TokensPerCorrect - 1) * 100
		c.TokenChangePct = &change
		c.ImprovedPointEstimate = change < 0 && len(c.RegressedTasks) == 0 && c.After.Correct >= c.Before.Correct
	}
	return c, nil
}

func taskCounts(report *Report) (map[string]int, error) {
	seen, correct, attempts := make(map[string]bool), make(map[string]int), make(map[string]int)
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		key := fmt.Sprintf("%s/%d", attempt.Task, attempt.Repeat)
		if seen[key] || attempt.Repeat < 1 || attempt.Repeat > report.Spec.Repeats {
			return nil, fmt.Errorf("duplicate or invalid attempt %s", key)
		}
		seen[key] = true
		attempts[attempt.Task]++
		if attempt.Score.Correct && attempt.Error == "" {
			correct[attempt.Task]++
		} else if _, exists := correct[attempt.Task]; !exists {
			correct[attempt.Task] = 0
		}
	}
	for task, count := range attempts {
		if count != report.Spec.Repeats {
			return nil, fmt.Errorf("task %s missing repeats", task)
		}
	}
	return correct, nil
}
