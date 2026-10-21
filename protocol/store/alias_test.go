package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// On a case-folding filesystem a path spelled differently reaches another
// document's files; the index holds the one spelling that was authorized.
func TestAliasedSpellingIsNotADocument(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, err := s.Write("/private/doc.md", []byte("# Secret\n"), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.BuildIndex(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("/private/doc.md", 0); err != nil {
		t.Fatalf("indexed spelling: %v", err)
	}
	if _, err := s.Get("/Private/doc.md", 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aliased spelling err = %v, want not exist", err)
	}
	if _, err := s.Get("/Private/doc.md", 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aliased version err = %v, want not exist", err)
	}
	// The write half only shows where the disk folds case.
	if _, err := os.Stat(filepath.Join(root, "Private", "doc.md")); err != nil {
		t.Skip("case-sensitive filesystem: the alias reaches nothing")
	}
	if _, err := s.Write("/Private/doc.md", []byte("# Replaced\n"), nil); !errors.Is(err, ErrPathAlias) {
		t.Fatalf("aliased write err = %v, want ErrPathAlias", err)
	}
	doc, err := s.Get("/private/doc.md", 0)
	if err != nil || string(doc.Content) != "# Secret\n" {
		t.Fatalf("document after aliased write = %q, %v", doc.Content, err)
	}
}
