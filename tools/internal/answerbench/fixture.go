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
}

// Evidence identifies the source passage required to support one answer field.
type Evidence struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
	Anchor  string `json:"anchor"`
	Quote   string `json:"quote"`
}

// Rubric is scorer-only data, never included in reader input or served documents.
type Rubric struct {
	Answer       map[string]json.RawMessage `json:"answer"`
	Evidence     map[string][]Evidence      `json:"evidence"`
	Abstain      bool                       `json:"abstain"`
	Supplemental map[string][]Evidence      `json:"supplemental,omitempty"`
}

// Fixture binds source snapshots, tasks, and separately held scoring rules.
type Fixture struct {
	Documents    []Document
	Tasks        []Task
	Rubrics      map[string]Rubric
	Hashes       map[string]string
	StoreRoot    string
	VersionCount int
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
	docs := make(map[string]bool)
	for _, doc := range f.Documents {
		if !strings.HasPrefix(doc.Path, "/") || !strings.HasSuffix(doc.Path, ".md") || path.Clean(doc.Path) != doc.Path || docs[doc.Path] || len(doc.Versions) == 0 {
			return fmt.Errorf("invalid or duplicate document %q", doc.Path)
		}
		docs[doc.Path] = true
	}
	seen := make(map[string]bool)
	for _, task := range f.Tasks {
		if task.ID == "" || path.Base(task.ID) != task.ID || seen[task.ID] || task.Question == "" || len(task.Fields) == 0 {
			return fmt.Errorf("invalid or duplicate task %q", task.ID)
		}
		seen[task.ID] = true
		if err := f.validateRubric(task); err != nil {
			return err
		}
	}
	if len(f.Tasks) == 0 || len(f.Rubrics) != len(f.Tasks) || !docs["/index.md"] {
		return errors.New("fixture needs tasks, matching rubrics, and /index.md")
	}
	return nil
}

func (f *Fixture) validateRubric(task Task) error {
	rubric, ok := f.Rubrics[task.ID]
	if !ok {
		return fmt.Errorf("task %s has no rubric", task.ID)
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
		if _, ok := rubric.Answer[field]; !ok || len(rubric.Evidence[field]) == 0 {
			return fmt.Errorf("task %s: missing answer/evidence for %s", task.ID, field)
		}
		for _, evidence := range rubric.Evidence[field] {
			if err := f.validateEvidence(task.ID, evidence); err != nil {
				return err
			}
		}
	}
	for field, evidence := range rubric.Supplemental {
		if _, ok := rubric.Answer[field]; !ok {
			return fmt.Errorf("task %s: supplemental evidence for unknown field %s", task.ID, field)
		}
		for _, e := range evidence {
			if err := f.validateEvidence(task.ID, e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *Fixture) validateEvidence(taskID string, evidence Evidence) error {
	body, ok := f.Section(evidence)
	if !ok || evidence.Quote == "" || !strings.Contains(body, evidence.Quote) {
		return fmt.Errorf("task %s: evidence not in source: %+v", taskID, evidence)
	}
	return nil
}

// Section resolves only revisions and anchors present in the frozen corpus.
func (f *Fixture) Section(e Evidence) (string, bool) {
	if f.StoreRoot != "" {
		doc, err := store.New(f.StoreRoot).Get(e.Path, e.Version)
		if err != nil {
			return "", false // An unreadable source cannot support the rubric or a citation.
		}
		if e.Anchor == "" {
			return string(doc.Content), true
		}
		return mdoutline.Section(string(doc.Content), e.Anchor)
	}
	for _, doc := range f.Documents {
		if doc.Path != e.Path || e.Version < 1 || e.Version > len(doc.Versions) {
			continue
		}
		body := doc.Versions[e.Version-1]
		if e.Anchor == "" {
			return body, true
		}
		return mdoutline.Section(body, e.Anchor)
	}
	return "", false
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
