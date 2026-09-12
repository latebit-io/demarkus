package answerbench

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/store"
)

func storedEvidenceFixture(t *testing.T) (fixture Fixture, questionsDir string) {
	t.Helper()
	f := testFixture(t)
	root := filepath.Join(t.TempDir(), "corpus")
	if err := seed(root, &f); err != nil {
		t.Fatal(err)
	}
	questions := t.TempDir()
	if err := writeNewJSON(filepath.Join(questions, "tasks.json"), f.Tasks); err != nil {
		t.Fatal(err)
	}
	if err := writeNewJSON(filepath.Join(questions, "rubric.json"), f.Rubrics); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadStoreFixture(t.Context(), root, questions)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, questions
}

func TestKnownStorageFailuresPropagateThroughScoringAndRescore(t *testing.T) {
	f, _ := storedEvidenceFixture(t)
	trace := validAnswerTrace(t, &f, "q1")
	trace.Usage.Input = 100
	report := Report{Spec: RunSpec{Hashes: f.Hashes, Port: 16319, ExpectedAttempts: 1}, Attempts: []Attempt{{Task: "q1", Trace: trace}}}
	report.summarize()
	before, after := filepath.Join(t.TempDir(), "before.json"), filepath.Join(t.TempDir(), "after.json")
	if err := writeNewJSON(before, report); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Evidence{{Path: "/unknown.md", Version: 1}, {Path: "/ops/retention.md", Version: 999}} {
		section, err := f.Section(e)
		if err != nil || section.Found {
			t.Fatalf("unknown citation should be unsupported: %+v %v", section, err)
		}
	}
	path, err := store.New(f.StoreRoot).VersionFilePath("/ops/retention.md", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Section(Evidence{Path: "/ops/retention.md", Version: 2}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost known version was hidden: %v", err)
	}
	if _, err := f.Score("q1", &trace, "127.0.0.1:16319"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("storage failure became a score: %v", err)
	}
	if err := f.Validate(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation hid storage failure: %v", err)
	}
	if _, err := RescoreWithFixture(before, after, &f); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rescore hid storage failure: %v", err)
	}
	if _, err := os.Stat(after); !os.IsNotExist(err) {
		t.Fatalf("failed rescore created output: %v", err)
	}
}

func TestRunSurfacesLostIndexBeforeReadiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell process fixture")
	}
	f, questions := storedEvidenceFixture(t)
	index, err := store.New(f.StoreRoot).VersionFilePath("/index.md", f.Latest("/index.md"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fake-opencode")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\nrm -- " + strconv.Quote(index) + " && printf '1.18.30\\n'\nelse\nexit 0\nfi\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{OpenCode: bin, Server: bin, MCP: bin, Proxy: bin, Corpus: f.StoreRoot, Questions: questions, Origin: "mark://fixture", Port: 16319, Model: "fixed/model", Steps: 1, Repeats: 1, Timeout: time.Second, Output: filepath.Join(t.TempDir(), "run"), Temp: t.TempDir()}
	report, err := Run(t.Context(), &cfg)
	if err == nil || !strings.Contains(err.Error(), "read frozen source /index.md/v1") || strings.Contains(err.Error(), "did not start") {
		t.Fatalf("lost index misreported: %v", err)
	}
	if report.Summary.UsageComplete || len(report.Attempts) != 0 {
		t.Fatalf("invalid run accepted: %+v", report.Summary)
	}
}
