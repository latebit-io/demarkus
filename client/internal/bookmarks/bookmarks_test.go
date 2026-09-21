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
	// A stored URL loads as its identity: the default port is not part of it (C14).
	if got[0].Title != "Hello" || got[0].URL != "mark://localhost/hello.md" || got[0].Date != "2026-03-04" {
		t.Fatalf("unexpected first bookmark: %+v", got[0])
	}
	if got[1].Title != "World" || got[1].URL != "mark://other/world.md" || got[1].Date != "" {
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

	// Reload from disk to ensure removal is persisted.
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Has("mark://localhost:6309/a.md") {
		t.Fatal("expected bookmark to be removed after reload")
	}
	if !s2.Has("mark://localhost:6309/b.md") {
		t.Fatal("expected other bookmark to remain after reload")
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

func TestBackslashBracketInTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	title := `Title with \] in it`
	if err := s.Add("mark://localhost:6309/test.md", title); err != nil {
		t.Fatal(err)
	}

	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.List()
	if len(got) != 1 {
		t.Fatalf("expected 1 bookmark, got %d", len(got))
	}
	if got[0].Title != title {
		t.Fatalf("expected title %q, got %q", title, got[0].Title)
	}
}

// A bookmark names a document, so every spelling of its URL is one bookmark:
// with or without the default port, in any host case (ADR 0005, ADR 0018).
func TestBookmarksAreKeyedByIdentity(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "bookmarks.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://Host.Example:6309/doc.md", "Doc"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://host.example/doc.md", "Doc again"); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 1 || got[0].URL != "mark://host.example/doc.md" || got[0].Title != "Doc" {
		t.Fatalf("bookmarks = %+v, want one entry under its identity", got)
	}
	for _, spelling := range []string{"mark://host.example/doc.md", "mark://host.example:6309/doc.md", "mark://HOST.example/doc.md"} {
		if !s.Has(spelling) {
			t.Errorf("Has(%q) = false", spelling)
		}
	}
	if err := s.Remove("mark://host.example:6309/doc.md"); err != nil || len(s.List()) != 0 {
		t.Errorf("Remove by another spelling left %+v, %v", s.List(), err)
	}
}

// A file written before bookmarks were canonical may hold one document twice.
func TestLoadMergesSpellingsOfOneDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	old := "# Bookmarks\n\n- [First](mark://host:6309/doc.md) — 2026-01-01\n- [Second](mark://host/doc.md) — 2026-02-01\n- [Other](mark://host:7000/doc.md) — 2026-03-01\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 2 || got[0].URL != "mark://host/doc.md" || got[0].Title != "First" || got[1].URL != "mark://host:7000/doc.md" {
		t.Errorf("bookmarks = %+v, want the first spelling kept and the other server untouched", got)
	}
}
