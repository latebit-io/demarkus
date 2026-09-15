package answerbench

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

type orderedExporter []corpusEntry

func (e orderedExporter) ExportDocs(ctx context.Context, fn func(string, store.StoredDocument) error) error {
	for _, entry := range e {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(entry.Path, entry.Document); err != nil {
			return err
		}
	}
	return nil
}

type generatedExporter int

func (e generatedExporter) ExportDocs(ctx context.Context, fn func(string, store.StoredDocument) error) error {
	document := store.StoredDocument{Versions: []store.StoredVersion{{Version: 1}}}
	for i := range int(e) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn("/"+strconv.Itoa(i), document); err != nil {
			return err
		}
	}
	return nil
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

func TestPackRejectsNilOptions(t *testing.T) {
	const corpusError = "pack requires root, archive, manifest, id and source"
	if _, err := PackCorpus(t.Context(), nil); err == nil || err.Error() != corpusError {
		t.Fatalf("PackCorpus error = %v, want %q", err, corpusError)
	}
	const exportError = "pack requires source exporter, archive, manifest, id and source"
	if _, err := PackExport(t.Context(), nil, store.New(t.TempDir())); err == nil || err.Error() != exportError {
		t.Fatalf("PackExport error = %v, want %q", err, exportError)
	}
}

func TestArchiveRestoreIgnoresExporterTraversalOrder(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	s := store.New(root)
	for _, path := range []string{"/docs/topic.md", "/docs/topic/detail.md"} {
		if _, err := s.WriteVersion(path, 0, []byte("# Topic\n"), nil); err != nil {
			t.Fatal(err)
		}
	}
	documents := exportedDocs(t, root)
	source := orderedExporter{
		{Path: "/docs/topic.md", Document: documents["/docs/topic.md"]},
		{Path: "/docs/topic/detail.md", Document: documents["/docs/topic/detail.md"]},
	}
	opts := PackOptions{Archive: filepath.Join(dir, "corpus.gz"), Manifest: filepath.Join(dir, "manifest.json"), ID: "ordered-v1", Source: "mark://ordered"}
	if _, err := PackExport(t.Context(), &opts, source); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreCorpus(t.Context(), opts.Manifest, opts.Archive, filepath.Join(dir, "restored")); err != nil {
		t.Fatal(err)
	}
}

func TestPackExportRejectsDuplicatePaths(t *testing.T) {
	dir, root := t.TempDir(), archiveSource(t)
	document := exportedDocs(t, root)["/index.md"]
	source := orderedExporter{{Path: "/index.md", Document: document}, {Path: "/index.md", Document: document}}
	opts := PackOptions{Archive: filepath.Join(dir, "corpus.gz"), Manifest: filepath.Join(dir, "manifest.json"), ID: "duplicate-v1", Source: "mark://duplicate"}
	if _, err := PackExport(t.Context(), &opts, source); err == nil || !strings.Contains(err.Error(), "duplicate corpus document /index.md") {
		t.Fatalf("duplicate export error = %v", err)
	}
	if _, err := os.Stat(opts.Archive); !os.IsNotExist(err) {
		t.Fatalf("failed duplicate archive retained: %v", err)
	}
	if _, err := os.Stat(opts.Manifest); !os.IsNotExist(err) {
		t.Fatalf("failed duplicate manifest retained: %v", err)
	}
}

func TestExportCorpusRejectsInvalidDocuments(t *testing.T) {
	valid := store.StoredDocument{Versions: []store.StoredVersion{{Version: 1}}}
	tests := []struct {
		name string
		path string
		doc  store.StoredDocument
		want string
	}{
		{name: "non-canonical path", path: "docs/x.md", doc: valid, want: "non-canonical corpus document path docs/x.md"},
		{name: "descending versions", path: "/docs/x.md", doc: store.StoredDocument{Versions: []store.StoredVersion{{Version: 2}, {Version: 1}}}, want: "versions not strictly ascending"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			stats, err := exportCorpus(t.Context(), orderedExporter{{Path: test.path, Document: test.doc}}, &output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid export error = %v, want %q", err, test.want)
			}
			if stats.Documents != 0 || output.Len() != 0 {
				t.Fatalf("invalid document encoded: stats = %+v, bytes = %d", stats, output.Len())
			}
		})
	}
}

func TestExportCorpusRejectsExcessiveDocuments(t *testing.T) {
	stats, err := exportCorpus(t.Context(), generatedExporter(maxCorpusDocuments+1), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "corpus exceeds 100000 documents") {
		t.Fatalf("excessive export error = %v", err)
	}
	if stats.Documents != maxCorpusDocuments {
		t.Fatalf("exported documents = %d, want %d", stats.Documents, maxCorpusDocuments)
	}
}

func TestArchiveRejectsCorruptionAndCleansOwnedOutput(t *testing.T) {
	dir := t.TempDir()
	opts := PackOptions{Root: archiveSource(t), Archive: filepath.Join(dir, "corpus.gz"), Manifest: filepath.Join(dir, "manifest.json"), ID: "test-v1", Source: "mark://fixture"}
	manifest, err := PackCorpus(t.Context(), &opts)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := manifest
	tooMany.Documents = maxCorpusDocuments + 1
	tooMany.Versions = tooMany.Documents
	if err := validateCorpusManifest(&tooMany); err == nil {
		t.Fatal("excessive document count accepted")
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
