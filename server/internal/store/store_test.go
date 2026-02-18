package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGet_FlatFile(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "doc.md"), []byte("# Hello"), 0o644)

	s := New(root)
	doc, err := s.Get("/doc.md", 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(doc.Content) != "# Hello" {
		t.Errorf("content = %q, want %q", doc.Content, "# Hello")
	}
	if doc.Version != 1 {
		t.Errorf("version = %d, want 1", doc.Version)
	}
}

func TestGet_VersionedFile(t *testing.T) {
	root := t.TempDir()
	versionsDir := filepath.Join(root, "versions")
	os.Mkdir(versionsDir, 0o755)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v1"), []byte("# V1"), 0o644)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v2"), []byte("# V2"), 0o644)
	// Current file (would be a symlink in production)
	os.WriteFile(filepath.Join(root, "doc.md"), []byte("# V2"), 0o644)

	s := New(root)

	// Get current version
	doc, err := s.Get("/doc.md", 0)
	if err != nil {
		t.Fatalf("Get current: %v", err)
	}
	if doc.Version != 2 {
		t.Errorf("current version = %d, want 2", doc.Version)
	}

	// Get specific version
	doc, err = s.Get("/doc.md", 1)
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	if string(doc.Content) != "# V1" {
		t.Errorf("v1 content = %q, want %q", doc.Content, "# V1")
	}
	if doc.Version != 1 {
		t.Errorf("v1 version = %d, want 1", doc.Version)
	}
}

func TestGet_NotFound(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	_, err := s.Get("/missing.md", 0)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestGet_PathTraversal(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	_, err := s.Get("/../etc/passwd", 0)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestGet_Directory(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "subdir"), 0o755)
	s := New(root)

	_, err := s.Get("/subdir", 0)
	if err == nil {
		t.Fatal("expected error for directory")
	}
}

func TestListDir(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.md"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(root, "b.md"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(root, ".hidden"), []byte("hidden"), 0o644)
	os.Mkdir(filepath.Join(root, "versions"), 0o755)

	s := New(root)
	entries, err := s.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2 (excluding .hidden and versions)", len(entries))
	}
}

func TestListDir_NotADirectory(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "file.md"), []byte("content"), 0o644)
	s := New(root)

	_, err := s.ListDir("/file.md")
	if err == nil {
		t.Fatal("expected error for file")
	}
}

func TestVersions_FlatFile(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "doc.md"), []byte("# Hello"), 0o644)

	s := New(root)
	versions, err := s.Versions("/doc.md")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions count = %d, want 1", len(versions))
	}
	if versions[0].Version != 1 {
		t.Errorf("version = %d, want 1", versions[0].Version)
	}
}

func TestVersions_MultipleVersions(t *testing.T) {
	root := t.TempDir()
	versionsDir := filepath.Join(root, "versions")
	os.Mkdir(versionsDir, 0o755)
	os.WriteFile(filepath.Join(root, "doc.md"), []byte("current"), 0o644)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v1"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v2"), []byte("v2"), 0o644)
	os.WriteFile(filepath.Join(versionsDir, "doc.md.v3"), []byte("v3"), 0o644)

	s := New(root)
	versions, err := s.Versions("/doc.md")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("versions count = %d, want 3", len(versions))
	}
	// Should be newest first.
	if versions[0].Version != 3 {
		t.Errorf("first version = %d, want 3", versions[0].Version)
	}
	if versions[2].Version != 1 {
		t.Errorf("last version = %d, want 1", versions[2].Version)
	}
}

func TestVersions_NotFound(t *testing.T) {
	root := t.TempDir()
	s := New(root)

	_, err := s.Versions("/missing.md")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestGetVersion_NotFound(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "doc.md"), []byte("current"), 0o644)

	s := New(root)
	_, err := s.Get("/doc.md", 99)
	if err == nil {
		t.Fatal("expected error for missing version")
	}
}
