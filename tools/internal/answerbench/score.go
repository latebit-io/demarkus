package answerbench

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/lookupexpand"
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

// Observations contain only source bodies actually returned to this reader.
func (f *Fixture) observed(trace *Trace, host string) map[string][]string {
	out := make(map[string][]string)
	for _, call := range trace.Calls {
		if call.Error != "" {
			continue
		}
		switch call.Name {
		case "fixture_mark_fetch":
			raw, _ := call.Input["url"].(string)
			e, err := location(raw, host)
			if err != nil {
				continue // Invalid/out-of-scope results cannot support a citation.
			}
			header, body, ok := strings.Cut(call.Output, "\n\n")
			if !ok || !strings.Contains(header, "status: ok") || strings.Contains(header, "mode: outline") {
				continue
			}
			var returned int
			for line := range strings.SplitSeq(header, "\n") {
				if value, ok := strings.CutPrefix(line, "version: "); ok {
					n, err := strconv.Atoi(value)
					if err != nil {
						continue // Invalid revision metadata cannot establish an observation.
					}
					returned = n
				}
			}
			if returned < 1 || (e.Version != 0 && e.Version != returned) {
				continue
			}
			e.Version = returned
			out[sourceKey(e)] = append(out[sourceKey(e)], normalize(body))
		case "fixture_mark_lookup":
			var current Evidence
			var body strings.Builder
			flush := func() {
				if current.Path != "" {
					out[sourceKey(current)] = append(out[sourceKey(current)], normalize(body.String()))
				}
			}
			for line := range strings.SplitSeq(call.Output, "\n") {
				raw, frame := strings.CutPrefix(line, lookupexpand.Delimiter+" ")
				if !frame {
					body.WriteString(line + "\n")
					continue
				}
				flush()
				body.Reset()
				current = Evidence{}
				if strings.HasPrefix(raw, "note: ") {
					continue
				}
				e, err := location(raw, host)
				if err != nil {
					continue // A malformed frame is not source evidence.
				}
				e.Version = f.Latest(e.Path)
				current = e
			}
			flush()
		}
	}
	return out
}

func sourceKey(e Evidence) string { return fmt.Sprintf("%s/v%d", e.Path, e.Version) }

// Score never calls the reader or reveals its rubric through a retrieval tool.
func (f *Fixture) Score(taskID string, trace *Trace, host string) Score {
	rubric, ok := f.Rubrics[taskID]
	if !ok {
		return Score{Reasons: []string{"missing rubric"}}
	}
	raw := strings.TrimSpace(trace.Final)
	if strings.HasPrefix(raw, "```json\n") && strings.HasSuffix(raw, "\n```") {
		raw = strings.TrimSuffix(strings.TrimPrefix(raw, "```json\n"), "\n```")
	}
	var answer Answer
	if err := decodeJSON([]byte(raw), &answer); err != nil {
		return Score{Reasons: []string{"invalid answer JSON: " + err.Error()}}
	}
	if len(trace.Calls) == 0 {
		return Score{Reasons: []string{"no retrieval attempted"}}
	}
	if rubric.Abstain {
		if answer.Abstain && len(answer.Answer) == 0 && len(answer.Citations) == 0 {
			return Score{Correct: true}
		}
		return Score{Reasons: []string{"expected abstention with no invented answer or citations"}}
	}
	if answer.Abstain || len(answer.Answer) != len(rubric.Answer) {
		return Score{Reasons: []string{"missing or extra answer fields"}}
	}
	for field, want := range rubric.Answer {
		var gotValue, wantValue any
		if err := json.Unmarshal(answer.Answer[field], &gotValue); err != nil {
			return Score{Reasons: []string{"missing/invalid field " + field}}
		}
		if err := json.Unmarshal(want, &wantValue); err != nil {
			return Score{Reasons: []string{"invalid rubric field " + field}}
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			return Score{Reasons: []string{"incorrect field " + field}}
		}
	}
	return f.scoreCitations(&answer, &rubric, trace, host)
}

func (f *Fixture) scoreCitations(answer *Answer, rubric *Rubric, trace *Trace, host string) Score {
	observed := f.observed(trace, host)
	covered := make(map[string]bool)
	for _, citation := range answer.Citations {
		e, err := location(citation.URL, host)
		if err != nil || e.Version == 0 || e.Anchor == "" || citation.Quote == "" {
			return Score{Reasons: []string{"citation lacks valid immutable source/section/quote"}}
		}
		body, exists := f.Section(e)
		quote := normalize(citation.Quote)
		if !exists || !strings.Contains(normalize(body), quote) {
			return Score{Reasons: []string{"quote absent from cited source section"}}
		}
		seen := false
		for _, text := range observed[sourceKey(e)] {
			seen = seen || strings.Contains(text, quote)
		}
		if !seen {
			return Score{Reasons: []string{"quoted evidence was not returned to reader"}}
		}
		supports := false
		for i, required := range rubric.Evidence[citation.Field] {
			if supportsEvidence(e, quote, required) {
				covered[fmt.Sprintf("%s:%d", citation.Field, i)] = true
				supports = true
			}
		}
		for _, supplemental := range rubric.Supplemental[citation.Field] {
			if supportsEvidence(e, quote, supplemental) {
				supports = true // Optional support never replaces mandatory coverage.
			}
		}
		if !supports {
			return Score{Reasons: []string{"citation does not support its answer field"}}
		}
	}
	for field, evidence := range rubric.Evidence {
		for i := range evidence {
			if !covered[fmt.Sprintf("%s:%d", field, i)] {
				return Score{Reasons: []string{"missing supporting citation for " + field}}
			}
		}
	}
	return Score{Correct: true}
}

func supportsEvidence(observed Evidence, quote string, required Evidence) bool {
	return sourceKey(observed) == sourceKey(required) && strings.Contains(quote, normalize(required.Quote))
}
