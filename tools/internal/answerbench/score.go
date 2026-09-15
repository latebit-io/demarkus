package answerbench

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol"
)

// Citation ties an answer field to an immutable observed source passage.
type Citation struct {
	Field string `json:"field"`
	URL   string `json:"url"`
	Quote string `json:"quote"`
}

// Answer keeps factual values separate from supporting citations and abstention.
type Answer struct {
	Answer    map[string]json.RawMessage `json:"answer"`
	Citations []Citation                 `json:"citations"`
	Abstain   bool                       `json:"abstain"`
	Outcome   string                     `json:"outcome,omitempty"`
}

// Score requires both exact factual values and complete citation support.
type Score struct {
	Correct                bool     `json:"correct"`
	DimensionsRecorded     bool     `json:"dimensions_recorded,omitempty"`
	AnswerCorrect          bool     `json:"answer_correct,omitempty"`
	EvidenceSufficient     bool     `json:"evidence_sufficient,omitempty"`
	CitationsValid         bool     `json:"citations_valid,omitempty"`
	ScopeComplete          bool     `json:"scope_complete,omitempty"`
	ContradictionsObserved []string `json:"contradictions_observed,omitempty"`
	Reasons                []string `json:"reasons,omitempty"`
}

var versionPath = regexp.MustCompile(`^(/.*\.md)/v([1-9]\d*)$`)

const scoringVersion = "section-provenance-v2"
const independentScoringVersionV1 = "independent-evidence-v1"
const independentScoringVersion = "independent-evidence-v2"

func validIndependentScoringVersion(version string) bool {
	return version == independentScoringVersionV1 || version == independentScoringVersion
}

func location(raw, host string) (Evidence, error) {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.User != nil || (u.Host != "" && strings.TrimSuffix(u.Host, ":6309") != strings.TrimSuffix(host, ":6309")) || (u.Scheme != "" && u.Scheme != "mark") || !strings.HasPrefix(u.Path, "/") {
		return Evidence{}, fmt.Errorf("invalid fixture location %q", raw)
	}
	e := Evidence{Path: u.Path, Anchor: u.Fragment}
	if parts := versionPath.FindStringSubmatch(u.Path); parts != nil {
		e.Path = parts[1]
		e.Version, err = strconv.Atoi(parts[2])
		if err != nil {
			return Evidence{}, fmt.Errorf("parse version: %w", err)
		}
	}
	return e, nil
}

func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

func sourceKey(e Evidence) string {
	key := fmt.Sprintf("%s/v%d", e.Path, e.Version)
	if e.Anchor != "" {
		key += "#" + e.Anchor
	}
	return key
}

func incorrect(reason string) (Score, error) { return Score{Reasons: []string{reason}}, nil }

// Score never calls the reader or reveals its rubric through a retrieval tool.
func (f *Fixture) Score(taskID string, trace *Trace, host string) (Score, error) {
	for _, task := range f.Tasks {
		if task.ID == taskID && task.Scope != "" {
			return f.scoreScoped(task, trace, host)
		}
	}
	return f.scoreLegacy(taskID, trace, host)
}

func (f *Fixture) scoreLegacy(taskID string, trace *Trace, host string) (Score, error) {
	rubric, ok := f.Rubrics[taskID]
	if !ok {
		return Score{}, fmt.Errorf("task %s: missing rubric", taskID)
	}
	raw := strings.TrimSpace(trace.Final)
	if strings.HasPrefix(raw, "```json\n") && strings.HasSuffix(raw, "\n```") {
		raw = strings.TrimSuffix(strings.TrimPrefix(raw, "```json\n"), "\n```")
	}
	var answer Answer
	if err := decodeJSON([]byte(raw), &answer); err != nil {
		return incorrect("invalid answer JSON: " + err.Error())
	}
	if len(trace.Calls) == 0 {
		return incorrect("no retrieval attempted")
	}
	if rubric.Abstain {
		if answer.Abstain && len(answer.Answer) == 0 && len(answer.Citations) == 0 {
			return Score{Correct: true}, nil
		}
		return incorrect("expected abstention with no invented answer or citations")
	}
	if answer.Abstain || len(answer.Answer) != len(rubric.Answer) {
		return incorrect("missing or extra answer fields")
	}
	for field, want := range rubric.Answer {
		var gotValue, wantValue any
		if err := json.Unmarshal(answer.Answer[field], &gotValue); err != nil {
			return incorrect("missing/invalid field " + field)
		}
		if err := json.Unmarshal(want, &wantValue); err != nil {
			return Score{}, fmt.Errorf("invalid rubric field %s: %w", field, err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			return incorrect("incorrect field " + field)
		}
	}
	return f.scoreCitations(&answer, &rubric, trace, host)
}

func (f *Fixture) scoreScoped(task Task, trace *Trace, host string) (Score, error) {
	rubric := f.Rubrics[task.ID]
	answer, err := parseAnswer(trace.Final)
	if err != nil {
		return Score{DimensionsRecorded: true, Reasons: []string{"invalid answer JSON: " + err.Error()}}, nil
	}
	score := Score{DimensionsRecorded: true}
	if len(trace.Calls) == 0 {
		score.Reasons = []string{"no retrieval attempted"}
		return score, nil
	}
	score.ContradictionsObserved, err = f.observedContradictions(&rubric, trace, host)
	if err != nil {
		return Score{}, err
	}
	score.AnswerCorrect = answer.Outcome == rubric.Outcome
	switch rubric.Outcome {
	case "answered":
		score.AnswerCorrect = score.AnswerCorrect && !answer.Abstain && exactAnswer(answer.Answer, rubric.Answer)
		assessment, err := f.assessCitations(&answer, &rubric, trace, host)
		if err != nil {
			return Score{}, err
		}
		score.CitationsValid = assessment.valid
		score.EvidenceSufficient = assessment.sufficient
		score.ScopeComplete = completionObserved(rubric.Completion, trace)
		score.Reasons = assessment.reasons
		score.Correct = score.AnswerCorrect && score.CitationsValid && score.EvidenceSufficient
		if f.scoringVersion() == independentScoringVersionV1 {
			score.Correct = score.Correct && score.ScopeComplete
		}
	case "not-found", "incomplete":
		empty := answer.Abstain && len(answer.Answer) == 0 && len(answer.Citations) == 0
		score.AnswerCorrect = score.AnswerCorrect && empty
		matched := completionObserved(rubric.Completion, trace)
		score.EvidenceSufficient = matched
		score.CitationsValid = len(answer.Citations) == 0
		score.ScopeComplete = rubric.Outcome == "not-found" && matched
		score.Correct = score.AnswerCorrect && score.EvidenceSufficient && score.CitationsValid && (score.ScopeComplete || rubric.Outcome == "incomplete")
		if !matched {
			score.Reasons = []string{"required scope outcome was not observed"}
		}
	}
	if !score.AnswerCorrect && len(score.Reasons) == 0 {
		score.Reasons = []string{"answer values or outcome differ"}
	}
	return score, nil
}

func parseAnswer(raw string) (Answer, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```json\n") && strings.HasSuffix(raw, "\n```") {
		raw = strings.TrimSuffix(strings.TrimPrefix(raw, "```json\n"), "\n```")
	}
	var answer Answer
	err := decodeJSON([]byte(raw), &answer)
	return answer, err
}

func exactAnswer(got, want map[string]json.RawMessage) bool {
	if len(got) != len(want) {
		return false
	}
	for field, rawWant := range want {
		var gotValue, wantValue any
		if json.Unmarshal(got[field], &gotValue) != nil || json.Unmarshal(rawWant, &wantValue) != nil || !reflect.DeepEqual(gotValue, wantValue) {
			return false
		}
	}
	return true
}

func completionObserved(expected []Completion, trace *Trace) bool {
	if len(expected) == 0 || len(expected) > len(trace.Calls) {
		return false
	}
	assigned := make([]int, len(trace.Calls))
	for i := range assigned {
		assigned[i] = -1
	}
	for i := range expected {
		seen := make([]bool, len(trace.Calls))
		if !assignCompletion(i, expected, trace, assigned, seen) {
			return false
		}
	}
	return true
}

func assignCompletion(index int, expected []Completion, trace *Trace, assigned []int, seen []bool) bool {
	for i := range trace.Calls {
		if seen[i] {
			continue
		}
		call := &trace.Calls[i]
		completion := &expected[index]
		if !completionCallMatches(completion, call) || !completionResultMatches(completion, call) {
			continue
		}
		seen[i] = true
		if assigned[i] == -1 || assignCompletion(assigned[i], expected, trace, assigned, seen) {
			assigned[i] = index
			return true
		}
	}
	return false
}

func completionCallMatches(completion *Completion, call *ToolCall) bool {
	return call.Name == "fixture_"+completion.Tool && call.Input["url"] == completion.URL &&
		(completion.Query == "" || call.Input["query"] == completion.Query) &&
		(completion.Match == "" || call.Input["match"] == completion.Match)
}

func completionResultMatches(completion *Completion, call *ToolCall) bool {
	header := resultHeader(call.Output)
	if completion.Failure {
		return call.Error != "" || explicitFailure(header)
	}
	actualComplete := header["partial"] != "true" && header["complete"] != "false"
	if call.Error != "" || header["status"] != completion.Status || (completion.Complete == nil && !actualComplete) || (completion.Match != "" && header["match"] != completion.Match) {
		return false
	}
	if completion.Matches != nil && header["matches"] != strconv.Itoa(*completion.Matches) {
		return false
	}
	return completion.Complete == nil || actualComplete == *completion.Complete
}

func explicitFailure(header map[string]string) bool {
	if header["partial"] == "true" || header["complete"] == "false" {
		return true
	}
	switch header["status"] {
	case protocol.StatusUnauthorized, protocol.StatusNotPermitted, protocol.StatusConflict, protocol.StatusBadRequest, protocol.StatusServerError, protocol.StatusRateLimited:
		return true
	default:
		return false
	}
}

func resultHeader(output string) map[string]string {
	header, _, _ := strings.Cut(output, "\n\n")
	values := make(map[string]string)
	for line := range strings.SplitSeq(header, "\n") {
		if key, value, ok := strings.Cut(line, ": "); ok {
			values[key] = value
		}
	}
	return values
}

func (f *Fixture) scoreCitations(answer *Answer, rubric *Rubric, trace *Trace, host string) (Score, error) {
	assessment, err := f.assessCitations(answer, rubric, trace, host)
	if err != nil {
		return Score{}, err
	}
	return Score{Correct: assessment.valid && assessment.sufficient, Reasons: assessment.reasons}, nil
}

type citationAssessment struct {
	valid, sufficient bool
	reasons           []string
}

func (f *Fixture) assessCitations(answer *Answer, rubric *Rubric, trace *Trace, host string) (citationAssessment, error) {
	observed, err := f.observed(trace, host)
	if err != nil {
		return citationAssessment{}, err
	}
	assessment := citationAssessment{valid: true, sufficient: true}
	covered := make(map[string]bool)
	for _, citation := range answer.Citations {
		e, err := location(citation.URL, host)
		if err != nil || e.Version == 0 || e.Anchor == "" || citation.Quote == "" {
			assessment.valid = false
			assessment.reasons = append(assessment.reasons, "citation lacks valid immutable source/section/quote")
			continue
		}
		section, err := f.Section(e)
		if err != nil {
			return citationAssessment{}, err
		}
		quote := normalize(citation.Quote)
		if !section.Found || !strings.Contains(normalize(section.Text), quote) {
			assessment.valid = false
			assessment.reasons = append(assessment.reasons, "quote absent from cited source section")
			continue
		}
		if !observedQuote(observed, e, quote) {
			assessment.valid = false
			assessment.reasons = append(assessment.reasons, "quoted section evidence was not returned to reader")
			continue
		}
		supports := false
		for i, required := range rubric.Evidence[citation.Field] {
			if supportsEvidence(e, &section, quote, required) {
				covered[fmt.Sprintf("%s:%d", citation.Field, i)] = true
				supports = true
			}
		}
		for _, supplemental := range rubric.Supplemental[citation.Field] {
			if supportsEvidence(e, &section, quote, supplemental) {
				supports = true // Optional support never replaces mandatory coverage.
			}
		}
		if !supports {
			assessment.valid = false
			assessment.reasons = append(assessment.reasons, "citation does not support its answer field")
		}
	}
	fields := make([]string, 0, len(rubric.Evidence))
	for field := range rubric.Evidence {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	for _, field := range fields {
		evidence := rubric.Evidence[field]
		for i := range evidence {
			if !covered[fmt.Sprintf("%s:%d", field, i)] {
				assessment.sufficient = false
				assessment.reasons = append(assessment.reasons, "missing supporting citation for "+field)
			}
		}
	}
	return assessment, nil
}

func (f *Fixture) observedContradictions(rubric *Rubric, trace *Trace, host string) ([]string, error) {
	observed, err := f.observed(trace, host)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, evidence := range rubric.Contradictions {
		if observedQuote(observed, evidence, normalize(evidence.Quote)) {
			found = append(found, sourceKey(evidence))
		}
	}
	slices.Sort(found)
	return found, nil
}

func supportsEvidence(cited Evidence, section *SectionResult, quote string, required Evidence) bool {
	if cited.Path != required.Path || cited.Version != required.Version || !strings.Contains(quote, normalize(required.Quote)) {
		return false
	}
	if cited.Anchor == required.Anchor {
		return true
	}
	// Section reads include descendants. A parent citation is valid only when
	// it contains the required section and identifies its passage unambiguously.
	if strings.Count(normalize(section.Text), normalize(required.Quote)) != 1 {
		return false
	}
	for _, h := range mdoutline.Headings(section.body) {
		if h.Anchor == required.Anchor && h.Start >= section.start && h.End <= section.end {
			return true
		}
	}
	return false
}
