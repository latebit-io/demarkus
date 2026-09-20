package store

import (
	"strings"
	"testing"
)

// A directory whose name merely starts with two dots is inside the root.
func TestPruneUnderDotDotPrefixedDirectory(t *testing.T) {
	s := New(t.TempDir())
	meta := map[string]string{"retention": "1"}
	for i := range 3 {
		doc, err := s.Write("/..notes/a.md", []byte(strings.Repeat("x", i+1)), meta)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if doc.Prune != nil && doc.Prune.Err != nil {
			t.Fatalf("write %d: prune error: %v", i, doc.Prune.Err)
		}
	}
	versions, err := s.Versions("/..notes/a.md")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("versions kept = %d, want 1", len(versions))
	}
}

func TestPartialWalkErrorZeroValue(t *testing.T) {
	var e PartialWalkError
	if got := e.Error(); !strings.Contains(got, "0 entries") {
		t.Errorf("Error() = %q", got)
	}
}
