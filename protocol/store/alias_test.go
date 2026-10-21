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
	if _, err := s.ReadCurrent("/Private/doc.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aliased ReadCurrent err = %v, want not exist", err)
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

// ReadCurrent takes every field from the one version file the current
// pointer named, and refuses a current file that is not a pointer.
func TestReadCurrentPinsOneVersion(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, err := s.Write("/doc.md", []byte("# One\n"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write("/doc.md", []byte("# Two\n"), map[string]string{"tags": "x"}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.ReadCurrent("/doc.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 2 || string(doc.Content) != "# Two\n" || doc.Metadata["tags"] != "x" || doc.ETag == "" {
		t.Fatalf("ReadCurrent = %+v", doc)
	}
	got, err := s.Get("/doc.md", 0)
	if err != nil || got.Version != doc.Version || got.ETag != doc.ETag || !got.Modified.Equal(doc.Modified) {
		t.Fatalf("Get = %+v, %v; want the same document as ReadCurrent", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "flat.md"), []byte("# Flat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadCurrent("/flat.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("flat file err = %v, want not exist", err)
	}
	if _, err := s.ReadCurrent("/missing.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing err = %v, want not exist", err)
	}
}
