package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWalkCurrent(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	if _, err := s.Write("/a.md", []byte("# A\nbody"), map[string]string{"tags": "go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("/sub/b.md", []byte("# B"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("/archived.md", []byte("# Arch"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveResult("/archived.md", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("/legacy.md", []byte("# Legacy"), nil); err != nil {
		t.Fatal(err)
	}
	legacyFile := filepath.Join(dir, "versions", "legacy.md", "v1")
	legacyStored, err := os.ReadFile(legacyFile)
	if err != nil {
		t.Fatal(err)
	}
	legacyStored, err = SetArchived(legacyStored, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyFile, legacyStored, 0o644); err != nil {
		t.Fatal(err)
	}

	got := make(map[string]CurrentDoc)
	if err := s.WalkCurrent(func(d CurrentDoc) error {
		got[d.Path] = d
		return nil
	}); err != nil {
		t.Fatalf("WalkCurrent: %v", err)
	}

	if _, ok := got["/archived.md"]; ok {
		t.Error("archived document should be excluded from WalkCurrent")
	}
	if _, ok := got["/legacy.md"]; ok {
		t.Error("legacy archived tip should be excluded from WalkCurrent")
	}
	if len(got) != 2 {
		t.Fatalf("walked %d documents, want 2: %v", len(got), keysOf(got))
	}

	a, ok := got["/a.md"]
	if !ok {
		t.Fatal("/a.md not walked")
	}
	if a.Metadata["tags"] != "go" {
		t.Errorf("/a.md metadata = %v, want tags=go", a.Metadata)
	}
	if !strings.Contains(string(a.Body), "# A") || strings.Contains(string(a.Body), "version:") {
		t.Errorf("/a.md body should be store-frontmatter-stripped markdown, got %q", a.Body)
	}
	if got["/a.md"].Modified.IsZero() {
		t.Error("/a.md modified time not set")
	}

	if err := os.WriteFile(filepath.Join(dir, "versions", "legacy.md", archiveStateName), []byte("false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = make(map[string]CurrentDoc)
	if err := s.WalkCurrent(func(d CurrentDoc) error {
		got[d.Path] = d
		return nil
	}); err != nil {
		t.Fatalf("WalkCurrent after explicit false: %v", err)
	}
	if _, ok := got["/legacy.md"]; !ok || len(got) != 3 {
		t.Errorf("explicit false walk = %v, want legacy.md and three documents", keysOf(got))
	}
}

func keysOf(m map[string]CurrentDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBuildHashIndex_ReportsSkippedEntries pins that an unindexable entry
// (here a dangling current-version symlink) is reported, not swallowed, and
// does not prevent the rest of the root from being indexed.
func TestBuildHashIndex_ReportsSkippedEntries(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, err := s.Write("/good.md", []byte("good"), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("versions/missing.md/v1", filepath.Join(root, "dangling.md")); err != nil {
		t.Fatal(err)
	}

	err := s.BuildHashIndex()
	var partial *PartialWalkError
	if !errors.As(err, &partial) {
		t.Fatalf("BuildHashIndex err = %v, want *PartialWalkError", err)
	}
	if len(partial.Skipped) != 1 || filepath.Base(partial.Skipped[0].Path) != "dangling.md" {
		t.Errorf("skipped = %+v, want the dangling symlink", partial.Skipped)
	}
	if p, err := s.LookupHashResult(ContentHash([]byte("good"))); err != nil || p != "/good.md" {
		t.Errorf("LookupHash hit after partial walk = (%q, %v), want (/good.md, nil)", p, err)
	}
	_, err = s.LookupHashResult(ContentHash([]byte("missing")))
	var lookupPartial *PartialWalkError
	if !errors.As(err, &lookupPartial) {
		t.Errorf("LookupHash miss after partial walk err = %v, want *PartialWalkError", err)
	}
}

// TestBuildHashIndex_RootFailureIsFatal pins that a root the walk cannot open
// is a real error, not a partial walk with zero entries. The missing-root case
// is deterministic everywhere; the permission case needs a non-root uid.
func TestBuildHashIndex_RootFailureIsFatal(t *testing.T) {
	newRoot := func(t *testing.T) (string, *Store) {
		root := filepath.Join(t.TempDir(), "content")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		s := New(root)
		if _, err := s.Write("/doc.md", []byte("x"), nil); err != nil {
			t.Fatal(err)
		}
		return root, s
	}
	assertFatal := func(t *testing.T, s *Store) {
		t.Helper()
		err := s.BuildHashIndex()
		var partial *PartialWalkError
		if err == nil || errors.As(err, &partial) {
			t.Fatalf("BuildHashIndex err = %v, want a non-partial error", err)
		}
	}

	t.Run("missing root", func(t *testing.T) {
		root, s := newRoot(t)
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		assertFatal(t, s)
	})
	t.Run("unreadable root", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		root, s := newRoot(t)
		if err := os.Chmod(root, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(root, 0o755); err != nil {
				t.Errorf("restore root permissions: %v", err)
			}
		})
		assertFatal(t, s)
	})
}
