package retrievalbench

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
)

// QuestionResult is one scored question with its call log.
type QuestionResult struct {
	ID              string   `json:"id"`
	Category        Category `json:"category"`
	Query           string   `json:"query"`
	ExpectedPath    string   `json:"expected_path"`
	ExpectedAnchor  string   `json:"expected_anchor,omitempty"`
	Hit             bool     `json:"hit"`
	CallsToEvidence int      `json:"calls_to_evidence"`
	CallsTotal      int      `json:"calls_total"`
	Tokens          int      `json:"tokens"`
	LookupRank      int      `json:"lookup_rank"`
	ElapsedMS       float64  `json:"elapsed_ms"`
	Calls           []Call   `json:"calls"`
	// Error is set when the strategy failed; the question counts as a miss.
	Error string `json:"error,omitempty"`
}

// Summary aggregates one category (or "all").
type Summary struct {
	Category       string  `json:"category"`
	Questions      int     `json:"questions"`
	Hits           int     `json:"hits"`
	HitRate        float64 `json:"hit_rate"`
	MeanCallsToHit float64 `json:"mean_calls_to_evidence"`
	MeanCalls      float64 `json:"mean_calls_total"`
	MeanTokens     float64 `json:"mean_tokens"`
	MedianTokens   int     `json:"median_tokens"`
	P90Tokens      int     `json:"p90_tokens"`
	MeanElapsedMS  float64 `json:"mean_elapsed_ms"`
}

// Report is the JSON artifact of one run.
type Report struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Endpoint    string           `json:"endpoint"`
	Strategy    string           `json:"strategy"`
	Tokenizer   string           `json:"tokenizer"`
	Scope       string           `json:"scope"`
	Summaries   []Summary        `json:"summaries"`
	Questions   []QuestionResult `json:"questions"`
}

var summaryOrder = []Category{CategoryVocabulary, CategoryBody, CategoryDecision}

// Summarize aggregates per category in fixed order, then "all".
func Summarize(results []QuestionResult) []Summary {
	out := make([]Summary, 0, len(summaryOrder)+1)
	for _, cat := range summaryOrder {
		var subset []*QuestionResult
		for i := range results {
			if results[i].Category == cat {
				subset = append(subset, &results[i])
			}
		}
		if len(subset) > 0 {
			out = append(out, summarize(string(cat), subset))
		}
	}
	all := make([]*QuestionResult, len(results))
	for i := range results {
		all[i] = &results[i]
	}
	return append(out, summarize("all", all))
}

func summarize(name string, rs []*QuestionResult) Summary {
	s := Summary{Category: name, Questions: len(rs)}
	if len(rs) == 0 {
		return s
	}
	tokens := make([]int, 0, len(rs))
	var callsToHit, calls, elapsed float64
	for _, r := range rs {
		tokens = append(tokens, r.Tokens)
		calls += float64(r.CallsTotal)
		elapsed += r.ElapsedMS
		if r.Hit {
			s.Hits++
			callsToHit += float64(r.CallsToEvidence)
		}
	}
	n := float64(len(rs))
	s.HitRate = float64(s.Hits) / n
	if s.Hits > 0 {
		s.MeanCallsToHit = callsToHit / float64(s.Hits)
	}
	s.MeanCalls = calls / n
	s.MeanElapsedMS = elapsed / n
	sum := 0
	for _, t := range tokens {
		sum += t
	}
	s.MeanTokens = float64(sum) / n
	slices.Sort(tokens)
	s.MedianTokens = percentile(tokens, 0.5)
	s.P90Tokens = percentile(tokens, 0.9)
	return s
}

// percentile uses nearest-rank on an ascending slice.
func percentile(sorted []int, p float64) int {
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	idx = max(0, min(idx, len(sorted)-1))
	return sorted[idx]
}

// Markdown renders the summary table and the per-question table.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Strategy: %s. Endpoint: %s. Scope: %s. Tokenizer: %s. Generated: %s.\n\n",
		r.Strategy, r.Endpoint, r.Scope, r.Tokenizer, r.GeneratedAt.Format(time.RFC3339))
	b.WriteString("| Category | Questions | Hits | Hit rate | Calls to evidence (hits) | Calls total | Tokens mean | Tokens median | Tokens p90 | Elapsed ms |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, s := range r.Summaries {
		fmt.Fprintf(&b, "| %s | %d | %d | %.0f%% | %.1f | %.1f | %.0f | %d | %d | %.0f |\n",
			s.Category, s.Questions, s.Hits, s.HitRate*100, s.MeanCallsToHit, s.MeanCalls,
			s.MeanTokens, s.MedianTokens, s.P90Tokens, s.MeanElapsedMS)
	}
	b.WriteString("\n| ID | Category | Query | Target | Hit | Rank | Calls | Tokens | Elapsed ms | Error |\n")
	b.WriteString("|---|---|---|---|---|---:|---:|---:|---:|---|\n")
	for i := range r.Questions {
		q := &r.Questions[i]
		target := q.ExpectedPath
		if q.ExpectedAnchor != "" {
			target += "#" + q.ExpectedAnchor
		}
		hit := "miss"
		if q.Hit {
			hit = "hit"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d | %d | %d | %.0f | %s |\n",
			cell(q.ID), q.Category, cell(q.Query), cell(target), hit, q.LookupRank, q.CallsTotal, q.Tokens, q.ElapsedMS, cell(q.Error))
	}
	return b.String()
}

// Failed counts questions whose strategy errored.
func (r *Report) Failed() int {
	n := 0
	for i := range r.Questions {
		if r.Questions[i].Error != "" {
			n++
		}
	}
	return n
}

// cell keeps fixture text from splitting a Markdown table row.
func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// WriteJSON stores the report as indented JSON.
func WriteJSON(path string, r *Report) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
