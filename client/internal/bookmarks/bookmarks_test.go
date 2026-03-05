package bookmarks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got != 0 {
		t.Fatalf("expected 0 bookmarks, got %d", got)
	}
}

func TestLoadExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	content := "# Bookmarks\n\n- [Hello](mark://localhost:6309/hello.md) — 2026-03-04\n- [World](mark://other:6309/world.md)\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("expected 2 bookmarks, got %d", len(got))
	}
	if got[0].Title != "Hello" || got[0].URL != "mark://localhost:6309/hello.md" || got[0].Date != "2026-03-04" {
		t.Fatalf("unexpected first bookmark: %+v", got[0])
	}
	if got[1].Title != "World" || got[1].URL != "mark://other:6309/world.md" || got[1].Date != "" {
		t.Fatalf("unexpected second bookmark: %+v", got[1])
	}
}

func TestLoadEmptyPath(t *testing.T) {
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestAddAndHas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/hello.md", "Hello"); err != nil {
		t.Fatal(err)
	}
	if !s.Has("mark://localhost:6309/hello.md") {
		t.Fatal("expected bookmark to exist")
	}
	if s.Has("mark://localhost:6309/other.md") {
		t.Fatal("expected bookmark to not exist")
	}
}

func TestAddDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/hello.md", "Hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/hello.md", "Hello Again"); err != nil {
		t.Fatal(err)
	}
	if got := len(s.List()); got != 1 {
		t.Fatalf("expected 1 bookmark after duplicate add, got %d", got)
	}
}

func TestRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/a.md", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/b.md", "B"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("mark://localhost:6309/a.md"); err != nil {
		t.Fatal(err)
	}
	if s.Has("mark://localhost:6309/a.md") {
		t.Fatal("expected bookmark to be removed")
	}
	if !s.Has("mark://localhost:6309/b.md") {
		t.Fatal("expected other bookmark to remain")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/hello.md", "Hello"); err != nil {
		t.Fatal(err)
	}

	// Reload from disk
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.List()); got != 1 {
		t.Fatalf("expected 1 bookmark after reload, got %d", got)
	}
	if s2.List()[0].Title != "Hello" {
		t.Fatalf("unexpected title after reload: %s", s2.List()[0].Title)
	}
}

func TestRender(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Render()
	if got != "# Bookmarks\n\nNo bookmarks yet. Press `b` on any page to bookmark it.\n" {
		t.Fatalf("unexpected empty render: %q", got)
	}

	if err := s.Add("mark://localhost:6309/hello.md", "Hello"); err != nil {
		t.Fatal(err)
	}
	got = s.Render()
	if got == "" {
		t.Fatal("expected non-empty render")
	}
}

func TestSaveCreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	path := filepath.Join(dir, "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/test.md", "Test"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected bookmarks file to exist: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("expected file permissions %v, got %v", want, got)
	}
}

func TestBracketInTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://localhost:6309/test.md", "Hello [World]"); err != nil {
		t.Fatal(err)
	}

	// Reload and verify the title survives round-trip
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 bookmark, got %d", len(got))
	}
	if got[0].Title != "Hello [World]" {
		t.Fatalf("expected title %q, got %q", "Hello [World]", got[0].Title)
	}
}
