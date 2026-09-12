package answerbench

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
)

func archiveSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	s := store.New(root)
	for _, item := range []struct {
		path, body string
		expected   int
	}{
		{"/index.md", "# Index\n", 0}, {"/docs/a.md", "# First\n", 0}, {"/docs/a.md", "# Second\n", 1}, {"/old.md", "# Archived\n", 0},
	} {
		if _, err := s.WriteVersion(item.path, item.expected, []byte(item.body), map[string]string{"tags": "fixture", "importance": "0.7"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.ArchiveResult("/old.md", true); err != nil {
		t.Fatal(err)
	}
	return root
}

func exportedDocs(t *testing.T, root string) map[string]store.StoredDocument {
	t.Helper()
	result := make(map[string]store.StoredDocument)
	if err := store.New(root).ExportDocs(t.Context(), func(path string, doc store.StoredDocument) error {
		result[path] = doc
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestArchiveRoundTripAndDeterminism(t *testing.T) {
	dir, root := t.TempDir(), archiveSource(t)
	opts := PackOptions{Root: root, Archive: filepath.Join(dir, "corpus.gz"), Manifest: filepath.Join(dir, "manifest.json"), ID: "test-v1", Source: "mark://fixture"}
	manifest, err := PackCorpus(t.Context(), &opts)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Documents != 3 || manifest.ActiveDocuments != 2 || manifest.Versions != 4 {
		t.Fatalf("lost archive state or versions: %+v", manifest)
	}
	restored := filepath.Join(dir, "restored")
	if _, err := RestoreCorpus(t.Context(), opts.Manifest, opts.Archive, restored); err != nil {
		t.Fatal(err)
	}
	if err := store.DiffExports(exportedDocs(t, root), exportedDocs(t, restored)); err != nil {
		t.Fatal(err)
	}
	other := opts
	other.Archive, other.Manifest = filepath.Join(dir, "again.gz"), filepath.Join(dir, "again.json")
	again, err := PackCorpus(t.Context(), &other)
	if err != nil || again.SHA256 != manifest.SHA256 || again.CorpusSHA256 != manifest.CorpusSHA256 {
		t.Fatalf("non-deterministic snapshot: %+v, %v", again, err)
	}
	if _, err := RestoreCorpus(t.Context(), opts.Manifest, opts.Archive, restored); err == nil {
		t.Fatal("existing restore destination overwritten")
	}
	if _, err := PackCorpus(t.Context(), &opts); err == nil {
		t.Fatal("existing archive overwritten")
	}
}

func TestArchiveRejectsCorruptionAndCleansOwnedOutput(t *testing.T) {
	dir := t.TempDir()
	opts := PackOptions{Root: archiveSource(t), Archive: filepath.Join(dir, "corpus.gz"), Manifest: filepath.Join(dir, "manifest.json"), ID: "test-v1", Source: "mark://fixture"}
	manifest, err := PackCorpus(t.Context(), &opts)
	if err != nil {
		t.Fatal(err)
	}
	bad := manifest
	bad.CorpusSHA256 = digest([]byte("wrong corpus"))
	badPath := filepath.Join(dir, "bad-manifest.json")
	if err := writeNewJSON(badPath, bad); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "failed-restore")
	if _, err := RestoreCorpus(t.Context(), badPath, opts.Archive, output); err == nil {
		t.Fatal("wrong corpus fingerprint accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("partial restore retained: %v", err)
	}
	if err := os.WriteFile(opts.Archive, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreCorpus(t.Context(), opts.Manifest, opts.Archive, output); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opts.Archive, opts.Manifest = filepath.Join(dir, "cancelled.gz"), filepath.Join(dir, "cancelled.json")
	if _, err := PackCorpus(ctx, &opts); err == nil {
		t.Fatal("cancelled pack succeeded")
	}
	if _, err := os.Stat(opts.Archive); !os.IsNotExist(err) {
		t.Fatalf("partial archive retained: %v", err)
	}
}

func TestMetricsExportOmitsPrivateContentAndPreservesComparison(t *testing.T) {
	dir := t.TempDir()
	private, public := filepath.Join(dir, "private.json"), filepath.Join(dir, "metrics.json")
	report := comparisonReport()
	const secret = "PRIVATE-CONTENT-SENTINEL"
	for i := range report.Attempts {
		report.Attempts[i].Trace.Session = secret
		report.Attempts[i].Trace.Final = secret
		report.Attempts[i].Trace.Calls = []ToolCall{{Name: "fixture_mark_fetch", Input: map[string]any{"url": secret}, Output: secret}}
	}
	report.summarize()
	if err := writeNewJSON(private, report); err != nil {
		t.Fatal(err)
	}
	if err := ExportMetrics(private, public); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(public)
	if err != nil || bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("private content retained: %v", err)
	}
	var got Report
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	c, err := Compare(&report, &got)
	if err != nil || c.TokenChangePct == nil || *c.TokenChangePct != 0 || got.ResultTokens == nil || *got.ResultTokens <= 0 || !got.MetricsOnly {
		t.Fatalf("comparison changed after export: %+v, %v", c, err)
	}
	if _, err := Rescore(public, filepath.Join(dir, "regrade.json")); err == nil {
		t.Fatal("metrics-only baseline accepted for regrading")
	}
}
