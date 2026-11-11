package bookmarks

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
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

// The file is the user's: headings and notes around the list survive a rewrite.
func TestRewriteKeepsWhatAPersonWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	const written = "# My Hub\n\nReading list, newest last.\n\n- [A](mark://h/a.md) — 2026-01-01\n\n## Later\n\n- [B](mark://h/b.md)\n"
	if err := os.WriteFile(path, []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("mark://h/a.md"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://h/c.md", "C"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# My Hub\n\nReading list, newest last.\n\n\n## Later\n\n- [B](mark://h/b.md)\n- [C](mark://h/c.md) — " + s.List()[1].Date + "\n"
	if string(got) != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

// A long-lived store writes over the file as it is now, not as it was loaded.
func TestMutationKeepsAnotherProcessesBookmarks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	tui, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Add("mark://h/cli.md", "CLI"); err != nil {
		t.Fatal(err)
	}
	if err := tui.Add("mark://h/tui.md", "TUI"); err != nil {
		t.Fatal(err)
	}
	fresh, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Has("mark://h/cli.md") || !fresh.Has("mark://h/tui.md") || !tui.Has("mark://h/cli.md") {
		t.Errorf("bookmarks = %+v, want both processes' entries", fresh.List())
	}
}

// A failed write changes nothing: the store never shows what the disk lacks,
// the old file stands, and no temp file is left behind.
func TestFailedSaveLeavesMemoryAndFileAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "bookmarks.md")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("mark://h/kept.md", "Kept"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore %s: %v", dir, err)
		}
	})
	if err := s.Add("mark://h/lost.md", "Lost"); err == nil {
		t.Fatal("Add into a read-only directory succeeded")
	}
	if err := s.Remove("mark://h/kept.md"); err == nil {
		t.Fatal("Remove in a read-only directory succeeded")
	}
	if s.Has("mark://h/lost.md") || !s.Has("mark://h/kept.md") {
		t.Errorf("memory diverged from disk: %+v", s.List())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Errorf("file changed by a failed write: %q, %v", after, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if name := e.Name(); name != "bookmarks.md" && name != "bookmarks.md.lock" {
			t.Errorf("a failed write left %s behind", name)
		}
	}
}

// The empty-list hint is for the reader; an older store wrote it to the file.
func TestEmptyHintIsNeverStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	if err := os.WriteFile(path, []byte(header+"\n\n"+emptyHint+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Render(), emptyHint) {
		t.Error("an empty list renders without its hint")
	}
	if err := s.Add("mark://h/a.md", "A"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(got), emptyHint) {
		t.Errorf("file = %q, %v; the hint was stored", got, err)
	}
}

// Stores in several processes each read, change and rewrite the file: under
// the lock none of them drops another's bookmark.
func TestConcurrentStoresLoseNoBookmark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bookmarks.md")
	const writers = 16
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			s, err := Load(path)
			if err != nil {
				t.Error(err)
				return
			}
			if err := s.Add(fmt.Sprintf("mark://h/%d.md", i), "doc"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	fresh, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(fresh.List()); got != writers {
		t.Errorf("%d bookmarks survive, want %d", got, writers)
	}
}
