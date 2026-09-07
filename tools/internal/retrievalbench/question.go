// Package retrievalbench measures how many tool calls and result tokens an
// agent spends reaching a known document through the demarkus MCP tools.
// It drives the same tools an agent uses, so every number is what the
// agent would have paid.
package retrievalbench

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
)

//go:embed questions/soul.json
var soulQuestions []byte

// Category classifies how a question's vocabulary relates to the catalog.
type Category string

// Question categories. Vocabulary: a query term is in the target's tags or
// title. Body: the term appears only in a body. Decision: which ADR settled X.
const (
	CategoryVocabulary Category = "vocabulary"
	CategoryBody       Category = "body"
	CategoryDecision   Category = "decision"
)

// Question is one benchmark item: the lookup subject an agent would type and
// the document (optionally section) that answers it.
type Question struct {
	ID             string   `json:"id"`
	Category       Category `json:"category"`
	Query          string   `json:"query"`
	Question       string   `json:"question"`
	ExpectedPath   string   `json:"expected_path"`
	ExpectedAnchor string   `json:"expected_anchor,omitempty"`
}

// QuestionSet is a checked-in fixture: the lookup scope plus its questions.
type QuestionSet struct {
	Scope     string     `json:"scope"`
	Questions []Question `json:"questions"`
}

// DefaultQuestionSet returns the embedded soul fixture.
func DefaultQuestionSet() (QuestionSet, error) {
	return ParseQuestionSet(soulQuestions)
}

// LoadQuestionSet reads and validates a fixture file.
func LoadQuestionSet(path string) (QuestionSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return QuestionSet{}, fmt.Errorf("read questions %s: %w", path, err)
	}
	return ParseQuestionSet(raw)
}

// ParseQuestionSet decodes and validates fixture JSON.
func ParseQuestionSet(raw []byte) (QuestionSet, error) {
	var set QuestionSet
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return QuestionSet{}, fmt.Errorf("parse questions: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return QuestionSet{}, fmt.Errorf("parse questions: trailing content after the fixture object")
	}
	if err := set.Validate(); err != nil {
		return QuestionSet{}, err
	}
	return set, nil
}

// Validate rejects fixtures a run could not score: missing fields, duplicate
// ids, unknown categories, malformed paths or anchors.
func (s QuestionSet) Validate() error {
	if !strings.HasPrefix(s.Scope, "/") || !strings.HasSuffix(s.Scope, "/") {
		return fmt.Errorf("scope %q must start and end with /", s.Scope)
	}
	if len(s.Questions) == 0 {
		return fmt.Errorf("no questions")
	}
	seen := make(map[string]bool, len(s.Questions))
	for i := range s.Questions {
		q := &s.Questions[i]
		if err := s.Questions[i].validate(); err != nil {
			return fmt.Errorf("question %d (%s): %w", i, q.ID, err)
		}
		if seen[q.ID] {
			return fmt.Errorf("question %d: duplicate id %q", i, q.ID)
		}
		seen[q.ID] = true
	}
	return nil
}

func (q *Question) validate() error {
	switch {
	case q.ID == "":
		return fmt.Errorf("missing id")
	case q.Query == "":
		return fmt.Errorf("missing query")
	case q.Question == "":
		return fmt.Errorf("missing question")
	case !strings.HasPrefix(q.ExpectedPath, "/") || strings.Contains(q.ExpectedPath, "#"):
		return fmt.Errorf("expected_path %q must be a bare absolute path", q.ExpectedPath)
	case q.ExpectedAnchor != "" && q.ExpectedAnchor != mdoutline.Slug(q.ExpectedAnchor):
		return fmt.Errorf("expected_anchor %q is not slug form (%q)", q.ExpectedAnchor, mdoutline.Slug(q.ExpectedAnchor))
	}
	switch q.Category {
	case CategoryVocabulary, CategoryBody, CategoryDecision:
		return nil
	default:
		return fmt.Errorf("unknown category %q", q.Category)
	}
}
