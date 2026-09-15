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
	RegressedCategories   []string `json:"regressed_categories"`
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
	if err := validateReport(&report); err != nil {
		return report, err
	}
	return report, nil
}

func validateReport(report *Report) error {
	switch report.Format {
	case "":
		return validateLegacyReport(report)
	case reportFormatV2:
		return validateV2Report(report)
	default:
		return fmt.Errorf("unsupported answer report format %q", report.Format)
	}
}

func validateLegacyReport(report *Report) error {
	if report.Spec.Suite == "" {
		return fmt.Errorf("historical report lacks suite identity")
	}
	if (report.RescoredAt != nil || report.ScorerHash != "") && (report.RescoredFrom == "" || report.RescoredAt == nil || report.ScorerHash == "") {
		return fmt.Errorf("formatless report has incomplete modern rescore provenance")
	}
	if !reflect.DeepEqual(report.Lifecycle, Lifecycle{}) || report.Spec.Suite == "independent-answer-v1" || report.Spec.Dataset != "" || report.Spec.DatasetHash != "" || report.Spec.ReaderContractHash != "" || report.Binaries["proxy"] != "" {
		return fmt.Errorf("formatless report contains v2-only fields")
	}
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		if attempt.Phase != "" || attempt.ResultTokens != 0 || attempt.Score.DimensionsRecorded {
			return fmt.Errorf("formatless report contains v2-only attempt fields")
		}
	}
	return validateLegacySummaries(report)
}

func validateV2Report(report *Report) error {
	if err := validateV2Identity(report); err != nil {
		return err
	}
	if err := validateV2Attempts(report); err != nil {
		return err
	}
	if err := validateDatasetContract(report); err != nil {
		return err
	}
	if err := validateLifecycle(report); err != nil {
		return err
	}
	return validateReportSummaries(report)
}

func validateV2Identity(report *Report) error {
	if report.Spec.Suite == "" || report.Generated.IsZero() || report.Lifecycle.Attribution != "run-read-capability-v1" || report.Lifecycle.Endpoint == "" {
		return fmt.Errorf("%s report lacks required identity fields", reportFormatV2)
	}
	for _, key := range []string{"corpus", "tasks", "rubric"} {
		if report.Spec.Hashes[key] == "" {
			return fmt.Errorf("%s report lacks %s hash", reportFormatV2, key)
		}
	}
	if report.Spec.ExpectedAttempts < 1 || report.Spec.Repeats < 1 || report.Spec.PromptHash == "" || report.Spec.ConfigHash == "" || report.Spec.ScoringVersion == "" || report.Lifecycle.ToolProfile == "" || len(report.Lifecycle.ToolNames) == 0 {
		return fmt.Errorf("%s report lacks required run contract", reportFormatV2)
	}
	for _, key := range []string{"server", "mcp", "runner", "proxy"} {
		if report.Binaries[key] == "" {
			return fmt.Errorf("%s report lacks %s implementation hash", reportFormatV2, key)
		}
	}
	if len(report.Attempts) > report.Spec.ExpectedAttempts {
		return fmt.Errorf("report has more attempts than scheduled")
	}
	rescored := report.RescoredFrom != "" || report.RescoredAt != nil || report.ScorerHash != ""
	if rescored && (report.RescoredFrom == "" || report.RescoredAt == nil || report.ScorerHash == "") {
		return fmt.Errorf("report has incomplete rescore provenance")
	}
	return nil
}

func validateV2Attempts(report *Report) error {
	seen := make(map[string]bool, len(report.Attempts))
	taskCategories := make(map[string]string)
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		expectedPhase := "warm"
		if attempt.Repeat == 1 {
			expectedPhase = "cold"
		}
		if attempt.Task == "" || attempt.Category == "" || attempt.Repeat < 1 || attempt.Repeat > report.Spec.Repeats || attempt.Phase != expectedPhase || attempt.Requests < 0 || attempt.ResultTokens < 0 || attempt.Trace.Turns < 0 || attempt.Trace.Usage.Input < 0 || attempt.Trace.Usage.Output < 0 || attempt.Trace.Usage.Reasoning < 0 || attempt.Trace.Usage.Cache.Read < 0 || attempt.Trace.Usage.Cache.Write < 0 {
			return fmt.Errorf("report contains invalid attempt identity or accounting")
		}
		if report.Spec.Suite == "independent-answer-v1" && !attempt.Score.DimensionsRecorded {
			return fmt.Errorf("independent report lacks scoring dimensions")
		}
		key := fmt.Sprintf("%s/%d", attempt.Task, attempt.Repeat)
		if seen[key] {
			return fmt.Errorf("report contains duplicate attempt %s", key)
		}
		seen[key] = true
		if category, exists := taskCategories[attempt.Task]; exists && category != attempt.Category {
			return fmt.Errorf("task %s changed category between repeats", attempt.Task)
		}
		taskCategories[attempt.Task] = attempt.Category
		if attempt.Trace.Usage.Total != nil && *attempt.Trace.Usage.Total != attempt.Trace.Usage.Tokens() {
			return fmt.Errorf("report contains inconsistent token total")
		}
	}
	return nil
}

func validateDatasetContract(report *Report) error {
	if report.Spec.Suite == "independent-answer-v1" {
		if report.Spec.Dataset == "" || report.Spec.DatasetHash == "" || report.Spec.Hashes["dataset"] == "" || report.Spec.ReaderContractHash == "" {
			return fmt.Errorf("independent report lacks dataset identity")
		}
		if !validIndependentScoringVersion(report.Spec.ScoringVersion) || report.Lifecycle.ToolProfile != "scoped-direct-read-v1" {
			return fmt.Errorf("independent report lacks scoped scorer contract")
		}
	} else if report.Spec.Dataset != "" || report.Spec.DatasetHash != "" || report.Spec.Hashes["dataset"] != "" {
		return fmt.Errorf("non-independent report contains dataset identity")
	}
	if report.Spec.DatasetHash != "" && report.Spec.DatasetHash != report.Spec.Hashes["dataset"] {
		return fmt.Errorf("report dataset hashes disagree")
	}
	return nil
}

func validateLifecycle(report *Report) error {
	if report.Summary.UsageComplete && (!report.Lifecycle.EndpointFree || !report.Lifecycle.StartupVerified || !report.Lifecycle.ServerStayedUp || !report.Lifecycle.CleanupVerified || report.Lifecycle.Failure != "") {
		return fmt.Errorf("complete report lacks process-owned lifecycle evidence")
	}
	if report.Summary.UsageComplete && len(report.Attempts) != report.Spec.ExpectedAttempts {
		return fmt.Errorf("complete report lacks scheduled attempts")
	}
	if report.MetricsOnly && (report.SourceReport == "" || report.ResultTokens == nil || *report.ResultTokens < 0) {
		return fmt.Errorf("metrics-only report lacks source or result-token identity")
	}
	return nil
}

type legacySummary struct {
	Attempts         int
	Correct          int
	KnownTokens      int64
	UsageComplete    bool
	TokensPerCorrect *float64
}

func legacySummaryOf(summary *Summary) legacySummary {
	return legacySummary{summary.Attempts, summary.Correct, summary.KnownTokens, summary.UsageComplete, summary.TokensPerCorrect}
}

func validateLegacySummaries(report *Report) error {
	expected := Summarize(report.Attempts)
	if !report.Summary.UsageComplete {
		expected.UsageComplete = false
		expected.TokensPerCorrect = nil
	}
	if !reflect.DeepEqual(legacySummaryOf(&report.Summary), legacySummaryOf(&expected)) {
		return fmt.Errorf("historical report summary does not match attempts")
	}
	categories := make(map[string][]Attempt)
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		categories[attempt.Category] = append(categories[attempt.Category], *attempt)
	}
	if len(report.Categories) != len(categories) {
		return fmt.Errorf("historical report category summaries do not match attempts")
	}
	for category, attempts := range categories {
		actual, expected := report.Categories[category], Summarize(attempts)
		if !reflect.DeepEqual(legacySummaryOf(&actual), legacySummaryOf(&expected)) {
			return fmt.Errorf("historical report category %s does not match attempts", category)
		}
	}
	return nil
}

func validateReportSummaries(report *Report) error {
	expected := Summarize(report.Attempts)
	if !report.Summary.UsageComplete {
		expected.UsageComplete = false
		expected.TokensPerCorrect = nil
	}
	if !reflect.DeepEqual(report.Summary, expected) {
		return fmt.Errorf("report summary does not match attempts")
	}
	categories := make(map[string][]Attempt)
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		categories[attempt.Category] = append(categories[attempt.Category], *attempt)
	}
	expectedCategories := make(map[string]Summary, len(categories))
	for category, attempts := range categories {
		expectedCategories[category] = Summarize(attempts)
	}
	if !reflect.DeepEqual(report.Categories, expectedCategories) {
		return fmt.Errorf("report category summaries do not match attempts")
	}
	return nil
}

// Compare rejects mismatched experiments and incomplete spend accounting.
func Compare(before, after *Report) (Comparison, error) {
	if err := validateReport(before); err != nil {
		return Comparison{}, err
	}
	if err := validateReport(after); err != nil {
		return Comparison{}, err
	}
	return compare(before, after)
}

func compare(before, after *Report) (Comparison, error) {
	beforeCounts, afterCounts, err := validateComparisonCohorts(before, after)
	if err != nil {
		return Comparison{}, err
	}
	c := Comparison{Before: Summarize(before.Attempts), After: Summarize(after.Attempts), Note: "Point estimates only; repeat paired runs before claiming a reliable improvement."}
	if !c.Before.UsageComplete || !c.After.UsageComplete || !before.Summary.UsageComplete || !after.Summary.UsageComplete {
		return c, fmt.Errorf("incomplete token accounting; ratio would hide unknown spend")
	}
	if err := addTaskRegressions(&c, beforeCounts, afterCounts); err != nil {
		return c, err
	}
	addCategoryRegressions(&c, before.Attempts, after.Attempts)
	if c.Before.TokensPerCorrect != nil && c.After.TokensPerCorrect != nil && *c.Before.TokensPerCorrect > 0 {
		change := (*c.After.TokensPerCorrect / *c.Before.TokensPerCorrect - 1) * 100
		c.TokenChangePct = &change
		c.ImprovedPointEstimate = change < 0 && len(c.RegressedTasks) == 0 && c.After.Correct >= c.Before.Correct
	}
	return c, nil
}

func validateComparisonCohorts(before, after *Report) (beforeCounts, afterCounts map[string]int, err error) {
	beforeRescored := before.RescoredFrom != "" || before.RescoredAt != nil || before.ScorerHash != ""
	afterRescored := after.RescoredFrom != "" || after.RescoredAt != nil || after.ScorerHash != ""
	beforeVerified := before.RescoredFrom != "" && before.RescoredAt != nil && before.ScorerHash != ""
	afterVerified := after.RescoredFrom != "" && after.RescoredAt != nil && after.ScorerHash != ""
	if (beforeRescored && !beforeVerified) || (afterRescored && !afterVerified) {
		return nil, nil, fmt.Errorf("legacy rescored reports lack comparable scorer provenance")
	}
	if beforeRescored != afterRescored || (beforeRescored && before.ScorerHash != after.ScorerHash) {
		return nil, nil, fmt.Errorf("rescored cohorts require the same scorer implementation")
	}
	beforeSpec, afterSpec := before.Spec, after.Spec
	beforeSpec.ReaderPolicy = policyName(beforeSpec.ReaderPolicy)
	afterSpec.ReaderPolicy = policyName(afterSpec.ReaderPolicy)
	if !reflect.DeepEqual(beforeSpec, afterSpec) {
		return nil, nil, fmt.Errorf("incompatible model, settings, prompt, or fixture; do not compare these runs")
	}
	if before.Spec.ExpectedAttempts <= 0 || len(before.Attempts) != before.Spec.ExpectedAttempts || len(after.Attempts) != after.Spec.ExpectedAttempts {
		return nil, nil, fmt.Errorf("partial cohort; expected all scheduled attempts")
	}
	beforeCounts, err = taskCounts(before)
	if err != nil {
		return nil, nil, err
	}
	afterCounts, err = taskCounts(after)
	if err != nil {
		return nil, nil, err
	}
	if !reflect.DeepEqual(taskCategories(before.Attempts), taskCategories(after.Attempts)) {
		return nil, nil, fmt.Errorf("task categories changed between cohorts")
	}
	return beforeCounts, afterCounts, nil
}

func addTaskRegressions(c *Comparison, beforeCounts, afterCounts map[string]int) error {
	for task, count := range beforeCounts {
		afterCount, exists := afterCounts[task]
		if !exists {
			return fmt.Errorf("different task cohorts")
		}
		if afterCount < count {
			c.RegressedTasks = append(c.RegressedTasks, task)
		}
	}
	if len(beforeCounts) != len(afterCounts) {
		return fmt.Errorf("different task cohorts")
	}
	slices.Sort(c.RegressedTasks)
	return nil
}

func addCategoryRegressions(c *Comparison, before, after []Attempt) {
	beforeCategories, afterCategories := categoryCorrect(before), categoryCorrect(after)
	for category, count := range beforeCategories {
		if afterCategories[category] < count {
			c.RegressedCategories = append(c.RegressedCategories, category)
		}
	}
	slices.Sort(c.RegressedCategories)
}

func taskCategories(attempts []Attempt) map[string]string {
	categories := make(map[string]string)
	for i := range attempts {
		attempt := &attempts[i]
		categories[attempt.Task] = attempt.Category
	}
	return categories
}

func categoryCorrect(attempts []Attempt) map[string]int {
	counts := make(map[string]int)
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.Score.Correct && attempt.Error == "" {
			counts[attempt.Category]++
		}
	}
	return counts
}

func taskCounts(report *Report) (map[string]int, error) {
	seen, correct, attempts, categories := make(map[string]bool), make(map[string]int), make(map[string]int), make(map[string]string)
	for i := range report.Attempts {
		attempt := &report.Attempts[i]
		key := fmt.Sprintf("%s/%d", attempt.Task, attempt.Repeat)
		if seen[key] || attempt.Repeat < 1 || attempt.Repeat > report.Spec.Repeats {
			return nil, fmt.Errorf("duplicate or invalid attempt %s", key)
		}
		seen[key] = true
		if category, exists := categories[attempt.Task]; exists && category != attempt.Category {
			return nil, fmt.Errorf("task %s changed category between repeats", attempt.Task)
		}
		categories[attempt.Task] = attempt.Category
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
