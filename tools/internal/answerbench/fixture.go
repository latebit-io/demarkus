// Package answerbench measures model-driven retrieval against frozen source facts.
package answerbench

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/latebit-io/demarkus/client/mdoutline"
	"github.com/latebit-io/demarkus/protocol/store"
)

//go:embed fixtures/corpus.json
var corpusJSON []byte

//go:embed fixtures/tasks.json
var tasksJSON []byte

//go:embed fixtures/rubric.json
var rubricJSON []byte

// Document freezes body-only revisions and their publisher metadata.
type Document struct {
	Path     string            `json:"path"`
	Metadata map[string]string `json:"metadata"`
	Versions []string          `json:"versions"`
	Current  int               `json:"current_version,omitempty"`
}

// Task is the only fixture object passed to the reader.
type Task struct {
	ID       string            `json:"id"`
	Category string            `json:"category"`
	Question string            `json:"question"`
	Fields   map[string]string `json:"fields"`
	Scope    string            `json:"scope,omitempty"`
}

// Evidence identifies the source passage required to support one answer field.
type Evidence struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
	Anchor  string `json:"anchor"`
	Quote   string `json:"quote"`
}

// Completion is scorer-only proof that a scope was completed or failed visibly.
type Completion struct {
	Step     string `json:"step"`
	Tool     string `json:"tool"`
	URL      string `json:"url"`
	Query    string `json:"query,omitempty"`
	Match    string `json:"match,omitempty"`
	Status   string `json:"status,omitempty"`
	Matches  *int   `json:"matches,omitempty"`
	Complete *bool  `json:"complete,omitempty"`
	Failure  bool   `json:"failure,omitempty"`
}

// Rubric is scorer-only data, never included in reader input or served documents.
type Rubric struct {
	Answer         map[string]json.RawMessage `json:"answer"`
	Evidence       map[string][]Evidence      `json:"evidence"`
	Abstain        bool                       `json:"abstain"`
	Supplemental   map[string][]Evidence      `json:"supplemental,omitempty"`
	Outcome        string                     `json:"outcome,omitempty"`
	Completion     []Completion               `json:"completion,omitempty"`
	Contradictions []Evidence                 `json:"contradictions,omitempty"`
}

// Fixture binds source snapshots, tasks, and separately held scoring rules.
type Fixture struct {
	Documents      []Document
	Tasks          []Task
	Rubrics        map[string]Rubric
	Hashes         map[string]string
	StoreRoot      string
	VersionCount   int
	Dataset        *DatasetManifest
	storedVersions map[string]map[int]bool
}

func digest(raw []byte) string { return fmt.Sprintf("sha256-%x", sha256.Sum256(raw)) }

func decodeJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

// LoadFixture rejects inconsistent answer keys before any paid reader run.
func LoadFixture() (Fixture, error) {
	f := Fixture{Hashes: map[string]string{"corpus": digest(corpusJSON), "tasks": digest(tasksJSON), "rubric": digest(rubricJSON)}}
	for _, item := range []struct {
		name string
		raw  []byte
		out  any
	}{
		{"corpus", corpusJSON, &f.Documents}, {"tasks", tasksJSON, &f.Tasks}, {"rubric", rubricJSON, &f.Rubrics},
	} {
		if err := decodeJSON(item.raw, item.out); err != nil {
			return Fixture{}, fmt.Errorf("decode %s: %w", item.name, err)
		}
	}
	err := f.Validate()
	return f, err
}

// Validate checks source identity, task uniqueness, and rubric provenance.
func (f *Fixture) Validate() error {
	docs, err := f.validateDocuments()
	if err != nil {
		return err
	}
	scoped, err := f.validateTasks()
	if err != nil {
		return err
	}
	if f.StoreRoot != "" && scoped && f.Dataset == nil {
		return errors.New("scoped copied fixture requires dataset.json")
	}
	if f.StoreRoot != "" && !scoped && f.Dataset != nil {
		return errors.New("dataset.json requires scoped copied tasks")
	}
	for i := range f.Tasks {
		if err := f.validateRubric(f.Tasks[i]); err != nil {
			return err
		}
	}
	if len(f.Tasks) == 0 || len(f.Rubrics) != len(f.Tasks) || !docs["/index.md"] {
		return errors.New("fixture needs tasks, matching rubrics, and /index.md")
	}
	return nil
}

func (f *Fixture) validateDocuments() (map[string]bool, error) {
	docs := make(map[string]bool)
	for i := range f.Documents {
		doc := &f.Documents[i]
		if !strings.HasPrefix(doc.Path, "/") || !strings.HasSuffix(doc.Path, ".md") || path.Clean(doc.Path) != doc.Path || docs[doc.Path] || len(doc.Versions) == 0 {
			return nil, fmt.Errorf("invalid or duplicate document %q", doc.Path)
		}
		if f.StoreRoot == "" && doc.Current != 0 && doc.Current != len(doc.Versions) {
			return nil, fmt.Errorf("document %s: current version %d is not the seeded latest revision", doc.Path, doc.Current)
		}
		docs[doc.Path] = true
	}
	return docs, nil
}

func (f *Fixture) validateTasks() (bool, error) {
	seen := make(map[string]bool)
	scoped := -1
	for i := range f.Tasks {
		task := &f.Tasks[i]
		if task.ID == "" || path.Base(task.ID) != task.ID || seen[task.ID] || task.Category == "" || task.Question == "" || len(task.Fields) == 0 {
			return false, fmt.Errorf("invalid or duplicate task %q", task.ID)
		}
		if task.Scope != "" && (!strings.HasPrefix(task.Scope, "/") || path.Clean(task.Scope) != task.Scope || strings.Contains(task.Scope, "\\")) {
			return false, fmt.Errorf("task %s: invalid scope %q", task.ID, task.Scope)
		}
		isScoped := 0
		if task.Scope != "" {
			isScoped = 1
		}
		if scoped == -1 {
			scoped = isScoped
		} else if scoped != isScoped {
			return false, errors.New("fixture cannot mix legacy and scoped tasks")
		}
		for field, kind := range task.Fields {
			if field == "" || !validAnswerType(kind) {
				return false, fmt.Errorf("task %s: invalid field/type %q=%q", task.ID, field, kind)
			}
		}
		seen[task.ID] = true
	}
	return scoped == 1, nil
}

func (f *Fixture) validateRubric(task Task) error {
	rubric, ok := f.Rubrics[task.ID]
	if !ok {
		return fmt.Errorf("task %s has no rubric", task.ID)
	}
	if task.Scope != "" {
		if err := f.validateScopedRubric(task, &rubric); err != nil {
			return err
		}
		if rubric.Outcome != "answered" {
			return f.validateContradictions(task, &rubric)
		}
	}
	if rubric.Abstain {
		if len(rubric.Answer) != 0 || len(rubric.Evidence) != 0 || len(rubric.Supplemental) != 0 {
			return fmt.Errorf("task %s: abstention rubric contains an answer", task.ID)
		}
		return nil
	}
	if len(task.Fields) != len(rubric.Answer) || len(task.Fields) != len(rubric.Evidence) {
		return fmt.Errorf("task %s: fields and rubric differ", task.ID)
	}
	for field := range task.Fields {
		answer, ok := rubric.Answer[field]
		if !ok || len(rubric.Evidence[field]) == 0 {
			return fmt.Errorf("task %s: missing answer/evidence for %s", task.ID, field)
		}
		if !answerMatchesType(answer, task.Fields[field]) {
			return fmt.Errorf("task %s: answer for %s does not match %s", task.ID, field, task.Fields[field])
		}
		for _, evidence := range rubric.Evidence[field] {
			if err := f.validateEvidence(task, evidence); err != nil {
				return err
			}
		}
	}
	for field, evidence := range rubric.Supplemental {
		if _, ok := rubric.Answer[field]; !ok {
			return fmt.Errorf("task %s: supplemental evidence for unknown field %s", task.ID, field)
		}
		for _, e := range evidence {
			if err := f.validateEvidence(task, e); err != nil {
				return err
			}
		}
	}
	return f.validateContradictions(task, &rubric)
}

func (f *Fixture) validateContradictions(task Task, rubric *Rubric) error {
	for _, evidence := range rubric.Contradictions {
		if err := f.validateEvidence(task, evidence); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fixture) validateScopedRubric(task Task, rubric *Rubric) error {
	if rubric.Abstain || (rubric.Outcome != "answered" && rubric.Outcome != "not-found" && rubric.Outcome != "incomplete") {
		return fmt.Errorf("task %s: scoped rubric requires an explicit outcome", task.ID)
	}
	if rubric.Outcome == "answered" {
		if len(rubric.Completion) == 0 {
			return fmt.Errorf("task %s: answered rubric requires completion evidence", task.ID)
		}
	} else if len(rubric.Answer) != 0 || len(rubric.Evidence) != 0 || len(rubric.Supplemental) != 0 || len(rubric.Completion) == 0 {
		return fmt.Errorf("task %s: %s rubric requires only completion evidence", task.ID, rubric.Outcome)
	}
	for i := range rubric.Completion {
		if err := validateCompletion(task, rubric.Outcome, &rubric.Completion[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateCompletion(task Task, outcome string, completion *Completion) error {
	if completion.Step == "" || completion.Tool != "mark_lookup" || !withinScope(task.Scope, completion.URL) || completion.Query == "" {
		return fmt.Errorf("task %s: invalid completion key", task.ID)
	}
	switch outcome {
	case "answered":
		if completion.Failure || completion.Status != "ok" || completion.Match != "body" || completion.Complete == nil || !*completion.Complete {
			return fmt.Errorf("task %s: answered scope requires a complete body lookup", task.ID)
		}
	case "not-found":
		if completion.Failure || completion.Status != "ok" || completion.Match != "body" || completion.Matches == nil || *completion.Matches != 0 {
			return fmt.Errorf("task %s: absence requires a complete zero-result body lookup", task.ID)
		}
	case "incomplete":
		if !completion.Failure && (completion.Complete == nil || *completion.Complete) {
			return fmt.Errorf("task %s: incomplete outcome requires visible failure or partial scope", task.ID)
		}
	}
	return nil
}

func (f *Fixture) scoringVersion() string {
	if f.Dataset != nil {
		return f.Dataset.ScoringVersion
	}
	if len(f.Tasks) > 0 && f.Tasks[0].Scope != "" {
		return independentScoringVersion
	}
	return scoringVersion
}

func (f *Fixture) validateEvidence(task Task, evidence Evidence) error {
	if evidence.Anchor == "" || !withinScope(task.Scope, evidence.Path) {
		return fmt.Errorf("task %s: evidence lacks an anchor or escapes scope: %+v", task.ID, evidence)
	}
	section, err := f.Section(evidence)
	if err != nil {
		return fmt.Errorf("task %s: %w", task.ID, err)
	}
	if !section.Found || evidence.Quote == "" || !strings.Contains(section.Text, evidence.Quote) {
		return fmt.Errorf("task %s: evidence not in source: %+v", task.ID, evidence)
	}
	return nil
}

func validAnswerType(kind string) bool {
	return kind == "string" || kind == "number" || kind == "boolean" || kind == "array of strings"
}

func answerMatchesType(raw json.RawMessage, kind string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array of strings":
		values, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range values {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func withinScope(scope, docPath string) bool {
	if !strings.HasPrefix(docPath, "/") {
		return false
	}
	docPath = path.Clean(docPath)
	if scope == "" || scope == "/" {
		return true
	}
	scope = strings.TrimSuffix(path.Clean(scope), "/")
	return docPath == scope || strings.HasPrefix(docPath, scope+"/")
}

// SectionResult retains document-relative ranges for nested-section provenance.
type SectionResult struct {
	Text       string
	Found      bool
	body       string
	start, end int
}

// Section distinguishes absent evidence from failures reading known snapshot data.
func (f *Fixture) Section(e Evidence) (SectionResult, error) {
	body, found, err := f.documentBody(e)
	if err != nil || !found {
		return SectionResult{}, err
	}
	if e.Anchor == "" {
		return SectionResult{Text: body, Found: true, body: body, end: len(body)}, nil
	}
	for _, h := range mdoutline.Headings(body) {
		if h.Anchor == e.Anchor {
			return SectionResult{Text: body[h.Start:h.End], Found: true, body: body, start: h.Start, end: h.End}, nil
		}
	}
	return SectionResult{}, nil
}

func (f *Fixture) documentBody(e Evidence) (body string, found bool, err error) {
	if e.Version < 1 {
		return "", false, nil
	}
	if f.StoreRoot != "" {
		if f.storedVersions == nil {
			return "", false, errors.New("store fixture has no version inventory")
		}
		if !f.storedVersions[e.Path][e.Version] {
			return "", false, nil
		}
		doc, err := store.New(f.StoreRoot).Get(e.Path, e.Version)
		if err != nil {
			return "", false, fmt.Errorf("read frozen source %s/v%d: %w", e.Path, e.Version, err)
		}
		return string(doc.Content), true, nil
	}
	for _, doc := range f.Documents {
		if doc.Path != e.Path || e.Version < 1 || e.Version > len(doc.Versions) {
			continue
		}
		return doc.Versions[e.Version-1], true, nil
	}
	return "", false, nil
}

// Latest is safe for budgeted lookup observations because the server is read-only.
func (f *Fixture) Latest(docPath string) int {
	for _, doc := range f.Documents {
		if doc.Path == docPath {
			if doc.Current > 0 {
				return doc.Current
			}
			return len(doc.Versions)
		}
	}
	return 0
}

func (f *Fixture) selectTask(id string) error {
	if id == "" {
		return nil
	}
	for _, task := range f.Tasks {
		if task.ID == id {
			f.Tasks = []Task{task}
			return nil
		}
	}
	return fmt.Errorf("unknown task %q", id)
}
