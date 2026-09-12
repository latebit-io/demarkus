package answerbench

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
)

func TestStoreFixturePreservesHistoryAndDetectsChanges(t *testing.T) {
	root := t.TempDir()
	s := store.New(root)
	for _, item := range []struct {
		path, body string
		expected   int
	}{
		{"/index.md", "# Index\n", 0}, {"/policy.md", "# Policy\n\n## Limit\n\nKeep 3 versions.\n", 0}, {"/policy.md", "# Policy\n\n## Limit\n\nKeep 7 versions.\n", 1},
	} {
		if _, err := s.WriteVersion(item.path, item.expected, []byte(item.body), nil); err != nil {
			t.Fatal(err)
		}
	}
	questions := t.TempDir()
	for name, body := range map[string]string{
		"tasks.json":  `[{"id":"history","question":"What was the old limit?","fields":{"limit":"number"}}]`,
		"rubric.json": `{"history":{"answer":{"limit":3},"evidence":{"limit":[{"path":"/policy.md","version":1,"anchor":"limit","quote":"Keep 3 versions."}]}}}`,
	} {
		if err := os.WriteFile(filepath.Join(questions, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := LoadStoreFixture(t.Context(), root, questions)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Documents) != 2 || f.VersionCount != 3 || f.Latest("/policy.md") != 2 {
		t.Fatalf("history inventory changed: docs=%d versions=%d latest=%d", len(f.Documents), f.VersionCount, f.Latest("/policy.md"))
	}
	if section, ok := f.Section(Evidence{Path: "/policy.md", Version: 1, Anchor: "limit"}); !ok || section != "## Limit\n\nKeep 3 versions.\n" {
		t.Fatalf("historical evidence changed: %q", section)
	}
	cfg := Config{Corpus: root, Questions: questions}
	if err := verifyFrozenCorpus(&cfg, f.Hashes["corpus"]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteVersion("/policy.md", 2, []byte("# Changed\n"), nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyFrozenCorpus(&cfg, f.Hashes["corpus"]); err == nil {
		t.Fatal("changed corpus accepted")
	}
}
