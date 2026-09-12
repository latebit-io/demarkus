package answerbench

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
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
}

// Score requires both exact factual values and complete citation support.
type Score struct {
	Correct bool     `json:"correct"`
	Reasons []string `json:"reasons,omitempty"`
}

var versionPath = regexp.MustCompile(`^(/.*\.md)/v([1-9]\d*)$`)

const scoringVersion = "section-provenance-v2"

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

func (f *Fixture) scoreCitations(answer *Answer, rubric *Rubric, trace *Trace, host string) (Score, error) {
	observed, err := f.observed(trace, host)
	if err != nil {
		return Score{}, err
	}
	covered := make(map[string]bool)
	for _, citation := range answer.Citations {
		e, err := location(citation.URL, host)
		if err != nil || e.Version == 0 || e.Anchor == "" || citation.Quote == "" {
			return incorrect("citation lacks valid immutable source/section/quote")
		}
		section, err := f.Section(e)
		if err != nil {
			return Score{}, err
		}
		quote := normalize(citation.Quote)
		if !section.Found || !strings.Contains(normalize(section.Text), quote) {
			return incorrect("quote absent from cited source section")
		}
		if !observedQuote(observed, e, quote) {
			return incorrect("quoted section evidence was not returned to reader")
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
			return incorrect("citation does not support its answer field")
		}
	}
	for field, evidence := range rubric.Evidence {
		for i := range evidence {
			if !covered[fmt.Sprintf("%s:%d", field, i)] {
				return incorrect("missing supporting citation for " + field)
			}
		}
	}
	return Score{Correct: true}, nil
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
