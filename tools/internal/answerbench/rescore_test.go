package answerbench

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRescorePreservesOriginalAndSpend(t *testing.T) {
	f := testFixture(t)
	trace := validAnswerTrace(t, &f, "q1")
	trace.Usage.Input = 123
	report := Report{
		Generated: time.Now(),
		Spec:      RunSpec{Suite: "graph-answer-v1", Hashes: f.Hashes, Port: 16319, Repeats: 1, ExpectedAttempts: 1},
		Attempts:  []Attempt{{Task: "q1", Category: "direct", Repeat: 1, Trace: trace, Score: Score{Correct: false}}},
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

func TestDatasetRescoreRequiresAndUpdatesReaderContract(t *testing.T) {
	f, task, trace := scopedAnswerFixture(t, "q2")
	contract, err := readerContract("section-first")
	if err != nil {
		t.Fatal(err)
	}
	contractHash := digest([]byte(contract))
	f.Hashes["dataset"] = "new-dataset"
	f.Dataset = &DatasetManifest{ID: "dataset-v2", Source: "mark://fixture", ScoringVersion: independentScoringVersion, ToolProfile: "scoped-direct-read-v1", ReaderPolicy: "section-first", ReaderContractSHA256: contractHash}
	hashes := make(map[string]string, len(f.Hashes))
	maps.Copy(hashes, f.Hashes)
	hashes["dataset"] = "old-dataset"
	report := Report{
		Format: reportFormatV2, Generated: time.Now(),
		Spec:      RunSpec{Suite: "independent-answer-v1", Origin: "mark://fixture", Repeats: 1, ExpectedAttempts: 1, PromptHash: "prompt", ConfigHash: "config", ScoringVersion: independentScoringVersion, ReaderPolicy: "section-first", Dataset: "dataset-v1", DatasetHash: "old-dataset", ReaderContractHash: contractHash, Hashes: hashes},
		Binaries:  map[string]string{"server": "server", "mcp": "mcp", "runner": "runner", "proxy": "proxy"},
		Lifecycle: Lifecycle{Attribution: "run-read-capability-v1", Endpoint: "127.0.0.1:16319", EndpointFree: true, StartupVerified: true, ServerStayedUp: true, CleanupVerified: true, ToolProfile: "scoped-direct-read-v1", ToolNames: []string{"mark_fetch"}},
		Attempts:  []Attempt{{Task: task.ID, Category: task.Category, Repeat: 1, Phase: "cold", Trace: trace, Score: Score{DimensionsRecorded: true}}},
	}
	report.summarize()
	dir := t.TempDir()
	before, after := filepath.Join(dir, "before.json"), filepath.Join(dir, "after.json")
	if err := writeJSON(before, report); err != nil {
		t.Fatal(err)
	}
	rescored, err := RescoreWithFixture(before, after, &f)
	if err != nil {
		t.Fatal(err)
	}
	if rescored.Spec.DatasetHash != "new-dataset" || rescored.Spec.Hashes["dataset"] != "new-dataset" {
		t.Fatalf("dataset hashes not updated: %+v", rescored.Spec)
	}
	if err := validateReport(&rescored); err != nil {
		t.Fatalf("rescored report is invalid: %v", err)
	}
	report.Spec.ReaderContractHash = "other-contract"
	if err := writeJSON(before, report); err != nil {
		t.Fatal(err)
	}
	if _, err := RescoreWithFixture(before, filepath.Join(dir, "invalid.json"), &f); err == nil {
		t.Fatal("changed reader contract accepted")
	}
}
