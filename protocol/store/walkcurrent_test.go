package store

import (
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
	if err := s.Archive("/archived.md", true); err != nil {
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
}

func keysOf(m map[string]CurrentDoc) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
