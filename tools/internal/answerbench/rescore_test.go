package answerbench

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRescorePreservesOriginalAndSpend(t *testing.T) {
	f := testFixture(t)
	trace := validAnswerTrace(t, &f, "q1")
	trace.Usage.Input = 123
	report := Report{
		Spec:     RunSpec{Hashes: f.Hashes, Port: 16319, Repeats: 1, ExpectedAttempts: 1},
		Attempts: []Attempt{{Task: "q1", Repeat: 1, Trace: trace, Score: Score{Correct: false}}},
	}
	report.Spec.Hashes["rubric"] = "old-rubric"
	report.summarize()
	dir := t.TempDir()
	beforePath, afterPath := filepath.Join(dir, "before.json"), filepath.Join(dir, "rescored.json")
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(beforePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := Rescore(beforePath, afterPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.RescoredFrom != digest(raw) || after.Summary.KnownTokens != 123 || after.Summary.Correct != 1 {
		t.Fatalf("rescore changed spend or lost provenance: %+v", after.Summary)
	}
	original, err := os.ReadFile(beforePath)
	if err != nil || !bytes.Equal(original, raw) {
		t.Fatalf("original report changed: %v", err)
	}
	if _, err := Rescore(beforePath, afterPath); err == nil {
		t.Fatal("existing output overwritten")
	}
	report.Spec.Hashes["corpus"] = "different-corpus"
	if err := writeJSON(beforePath, report); err != nil {
		t.Fatal(err)
	}
	if _, err := Rescore(beforePath, filepath.Join(dir, "invalid.json")); err == nil {
		t.Fatal("changed corpus accepted for rescore")
	}
}
